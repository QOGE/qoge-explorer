package query

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/jackc/pgx/v5"
)

// TxOccurrence is one block that a transaction (specifically, one witness
// variant of it — see WTxID) has ever been recorded in. A txid can have
// more than one occurrence across a reorg; at most one is ever canonical at
// a time (blocks_height_canonical_uidx), but historical/orphaned
// occurrences remain queryable forever.
type TxOccurrence struct {
	BlockHash   string `json:"block_hash"`
	BlockHeight int64  `json:"block_height"`
	TxIndex     int    `json:"tx_index"`
	WTxID       string `json:"wtxid"`
	Canonical   bool   `json:"canonical"`
}

// WitnessItem is one item of an input's witness stack. DataHex is nil
// unless the raw witness data was explicitly requested — see
// docs/ARCHITECTURE.md §19 "P2QPK large-witness response policy": a P2QPK
// signature item is exactly 17,088 bytes, so this must never be embedded by
// default.
type WitnessItem struct {
	ItemIndex int     `json:"item_index"`
	SizeBytes int     `json:"size_bytes"`
	DataHex   *string `json:"data_hex,omitempty"`
}

// InputDetail is one transaction input (vin), preserved 1:1 with what
// internal/store persisted.
type InputDetail struct {
	VinIndex      int           `json:"vin_index"`
	CoinbaseHex   *string       `json:"coinbase_hex,omitempty"`
	PrevTxID      *string       `json:"prev_txid,omitempty"`
	PrevVoutIndex *int          `json:"prev_vout_index,omitempty"`
	ScriptSigHex  string        `json:"script_sig_hex"`
	Sequence      uint32        `json:"sequence"`
	Witness       []WitnessItem `json:"witness,omitempty"`
}

// OutputDetail is one transaction output (vout).
type OutputDetail struct {
	VoutIndex       int      `json:"vout_index"`
	ValueSatoshis   int64    `json:"value_sats"`
	ValueQOGE       string   `json:"value_qoge"`
	ScriptPubKeyHex string   `json:"script_pubkey_hex"`
	ScriptType      string   `json:"script_type"`
	Address         *string  `json:"address,omitempty"`
	WitnessVersion  *int     `json:"witness_version,omitempty"`
	WitnessProgram  *string  `json:"witness_program,omitempty"`
	Participants    []string `json:"participants,omitempty"`
	// Spent is nil when this output was never tracked in canonical UTXO
	// state at all — the genesis coinbase, or any script.IsUnspendable
	// output (e.g. OP_RETURN) — see ApplyBlock's "Core UTXO semantics"
	// doc comment in internal/store/apply.go. It is never fabricated as
	// false in that case.
	Spent *bool `json:"spent,omitempty"`
}

// TransactionDetail is a transaction's immutable body plus every block
// occurrence it has ever had.
type TransactionDetail struct {
	TxID        string         `json:"txid"`
	WTxID       string         `json:"wtxid"`
	Version     uint32         `json:"version"`
	LockTime    uint32         `json:"locktime"`
	Size        int            `json:"size"`
	VSize       int            `json:"vsize"`
	Weight      int            `json:"weight"`
	IsCoinbase  bool           `json:"is_coinbase"`
	FeeSatoshis *int64         `json:"fee_sats,omitempty"`
	FeeQOGE     *string        `json:"fee_qoge,omitempty"`
	Inputs      []InputDetail  `json:"inputs"`
	Outputs     []OutputDetail `json:"outputs"`
	Occurrences []TxOccurrence `json:"occurrences"`
}

// TransactionByTxID looks up a transaction by its non-witness identity
// (Core's GetHash(), RPC field "txid"). The witness variant shown
// (WTxID/Size/VSize/Weight/witness metadata) is the CANONICAL occurrence's
// variant if one exists, otherwise the most recently observed orphaned
// variant — see pickRepresentativeWTxID. Every read that makes up the
// response — the transaction body, its occurrences/canonical flags, the
// chosen variant, witness data, inputs, and outputs/utxo_state — comes
// from ONE read-only REPEATABLE READ snapshot (readTx), so a concurrent
// reorg can never produce a response mixing pre- and post-reorg state —
// see docs/ARCHITECTURE.md §19 "Multi-statement read consistency".
func (s *Store) TransactionByTxID(ctx context.Context, txid string, includeRawWitness bool) (TransactionDetail, error) {
	tx, done, err := s.readTx(ctx)
	if err != nil {
		return TransactionDetail{}, err
	}
	defer done()
	return transactionDetail(ctx, tx, txid, nil, includeRawWitness)
}

// TransactionByWTxID looks up a transaction by a specific witness variant's
// identity (Core's GetWitnessHash(), RPC field "hash"). Unlike
// TransactionByTxID, the variant shown is always exactly the one requested
// — canonical or not — never silently substituted for a different variant
// of the same txid. The wtxid->txid identity resolution and every
// subsequent detail read share the SAME read-only REPEATABLE READ
// snapshot — resolving the identity in a separate, earlier statement
// (as an initial version of this code did) would let a concurrent reorg
// land between identity resolution and detail reads.
func (s *Store) TransactionByWTxID(ctx context.Context, wtxid string, includeRawWitness bool) (TransactionDetail, error) {
	tx, done, err := s.readTx(ctx)
	if err != nil {
		return TransactionDetail{}, err
	}
	defer done()

	var txid string
	err = tx.QueryRow(ctx, `SELECT txid FROM transaction_variants WHERE wtxid = $1`, wtxid).Scan(&txid)
	if errors.Is(err, pgx.ErrNoRows) {
		return TransactionDetail{}, ErrNotFound
	}
	if err != nil {
		return TransactionDetail{}, fmt.Errorf("query: transaction variant %s: %w", wtxid, err)
	}
	return transactionDetail(ctx, tx, txid, &wtxid, includeRawWitness)
}

// transactionDetail builds a TransactionDetail for txid using q — the
// caller's read-only snapshot transaction. preferredWTxID, if non-nil,
// pins the shown variant to exactly that wtxid (TransactionByWTxID
// callers); nil lets pickRepresentativeWTxID choose (TransactionByTxID
// callers).
func transactionDetail(ctx context.Context, q querier, txid string, preferredWTxID *string, includeRawWitness bool) (TransactionDetail, error) {
	var d TransactionDetail
	var fee *int64
	err := q.QueryRow(ctx, `
		SELECT txid, version, locktime, is_coinbase, fee_satoshis FROM transactions WHERE txid = $1
	`, txid).Scan(&d.TxID, &d.Version, &d.LockTime, &d.IsCoinbase, &fee)
	if errors.Is(err, pgx.ErrNoRows) {
		return TransactionDetail{}, ErrNotFound
	}
	if err != nil {
		return TransactionDetail{}, fmt.Errorf("query: transaction %s: %w", txid, err)
	}
	fireSnapshotTestHook() // snapshot is now fixed as of this first statement
	if fee != nil {
		d.FeeSatoshis = fee
		q := chain.Amount(*fee).String()
		d.FeeQOGE = &q
	}

	occurrences, err := txOccurrences(ctx, q, txid)
	if err != nil {
		return TransactionDetail{}, err
	}
	d.Occurrences = occurrences

	wtxid, err := pickRepresentativeWTxID(txid, occurrences, preferredWTxID)
	if err != nil {
		return TransactionDetail{}, err
	}
	d.WTxID = wtxid

	if err := q.QueryRow(ctx, `
		SELECT size, vsize, weight FROM transaction_variants WHERE wtxid = $1
	`, wtxid).Scan(&d.Size, &d.VSize, &d.Weight); err != nil {
		return TransactionDetail{}, fmt.Errorf("query: transaction variant %s: %w", wtxid, err)
	}

	witnessByVin, err := witnessByVin(ctx, q, wtxid, includeRawWitness)
	if err != nil {
		return TransactionDetail{}, err
	}

	inputs, err := txInputs(ctx, q, txid, witnessByVin)
	if err != nil {
		return TransactionDetail{}, err
	}
	d.Inputs = inputs

	outputs, err := txOutputs(ctx, q, txid)
	if err != nil {
		return TransactionDetail{}, err
	}
	d.Outputs = outputs

	return d, nil
}

// pickRepresentativeWTxID chooses which witness variant a transaction-body
// response describes. preferred, when non-nil, wins outright (the wtxid
// lookup path). Otherwise: the canonical occurrence's wtxid if one exists,
// else the highest-block-height (most recently observed) orphaned
// occurrence's wtxid — deterministic even though occurrences is not
// required to be sorted by the caller. A txid with zero occurrences would
// violate internal/store's own invariant (every applied transaction gets a
// block_transactions row in the same call that inserts it) and is reported
// as an internal error rather than silently guessed at.
func pickRepresentativeWTxID(txid string, occurrences []TxOccurrence, preferred *string) (string, error) {
	if preferred != nil {
		return *preferred, nil
	}
	if len(occurrences) == 0 {
		return "", fmt.Errorf("query: transaction %s has no recorded block occurrence (inconsistent index state)", txid)
	}
	best := occurrences[0]
	for _, occ := range occurrences[1:] {
		if occ.Canonical && !best.Canonical {
			best = occ
			continue
		}
		if occ.Canonical == best.Canonical && occ.BlockHeight > best.BlockHeight {
			best = occ
		}
	}
	return best.WTxID, nil
}

func txOccurrences(ctx context.Context, q querier, txid string) ([]TxOccurrence, error) {
	rows, err := q.Query(ctx, `
		SELECT b.hash, bt.block_height, bt.tx_index, bt.wtxid, b.canonical
		FROM block_transactions bt
		JOIN blocks b ON b.hash = bt.block_hash
		WHERE bt.txid = $1
		ORDER BY b.canonical DESC, bt.block_height DESC
	`, txid)
	if err != nil {
		return nil, fmt.Errorf("query: transaction occurrences for %s: %w", txid, err)
	}
	defer rows.Close()

	var occs []TxOccurrence
	for rows.Next() {
		var o TxOccurrence
		if err := rows.Scan(&o.BlockHash, &o.BlockHeight, &o.TxIndex, &o.WTxID, &o.Canonical); err != nil {
			return nil, fmt.Errorf("query: transaction occurrences for %s: scan: %w", txid, err)
		}
		occs = append(occs, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: transaction occurrences for %s: %w", txid, err)
	}
	return occs, nil
}

// witnessByVin returns wtxid's witness stack, grouped by vin_index and
// ordered by item_index within each. Raw bytes are only fetched from
// PostgreSQL at all when includeRaw is true — a P2QPK signature item alone
// is 17,088 bytes, and this must never be pulled for an ordinary
// transaction-detail response (docs/ARCHITECTURE.md §19).
func witnessByVin(ctx context.Context, q querier, wtxid string, includeRaw bool) (map[int][]WitnessItem, error) {
	var rows pgx.Rows
	var err error
	if includeRaw {
		rows, err = q.Query(ctx, `
			SELECT vin_index, item_index, data FROM transaction_input_witness
			WHERE wtxid = $1 ORDER BY vin_index, item_index
		`, wtxid)
	} else {
		rows, err = q.Query(ctx, `
			SELECT vin_index, item_index, octet_length(data) FROM transaction_input_witness
			WHERE wtxid = $1 ORDER BY vin_index, item_index
		`, wtxid)
	}
	if err != nil {
		return nil, fmt.Errorf("query: witness for %s: %w", wtxid, err)
	}
	defer rows.Close()

	result := map[int][]WitnessItem{}
	for rows.Next() {
		var vin, itemIndex int
		var item WitnessItem
		if includeRaw {
			var data []byte
			if err := rows.Scan(&vin, &itemIndex, &data); err != nil {
				return nil, fmt.Errorf("query: witness for %s: scan: %w", wtxid, err)
			}
			item.SizeBytes = len(data)
			hexData := hex.EncodeToString(data)
			item.DataHex = &hexData
		} else {
			if err := rows.Scan(&vin, &itemIndex, &item.SizeBytes); err != nil {
				return nil, fmt.Errorf("query: witness for %s: scan: %w", wtxid, err)
			}
		}
		item.ItemIndex = itemIndex
		result[vin] = append(result[vin], item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: witness for %s: %w", wtxid, err)
	}
	return result, nil
}

func txInputs(ctx context.Context, q querier, txid string, witnessByVin map[int][]WitnessItem) ([]InputDetail, error) {
	rows, err := q.Query(ctx, `
		SELECT vin_index, prev_txid, prev_vout_index, coinbase, script_sig, sequence
		FROM transaction_inputs WHERE txid = $1 ORDER BY vin_index
	`, txid)
	if err != nil {
		return nil, fmt.Errorf("query: inputs for %s: %w", txid, err)
	}
	defer rows.Close()

	var inputs []InputDetail
	for rows.Next() {
		var in InputDetail
		var prevVout *int64
		var coinbase, scriptSig []byte
		var sequence int64
		if err := rows.Scan(&in.VinIndex, &in.PrevTxID, &prevVout, &coinbase, &scriptSig, &sequence); err != nil {
			return nil, fmt.Errorf("query: inputs for %s: scan: %w", txid, err)
		}
		if prevVout != nil {
			v := int(*prevVout)
			in.PrevVoutIndex = &v
		}
		if coinbase != nil {
			h := hex.EncodeToString(coinbase)
			in.CoinbaseHex = &h
		}
		in.ScriptSigHex = hex.EncodeToString(scriptSig)
		in.Sequence = uint32(sequence)
		in.Witness = witnessByVin[in.VinIndex]
		inputs = append(inputs, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: inputs for %s: %w", txid, err)
	}
	return inputs, nil
}

func txOutputs(ctx context.Context, q querier, txid string) ([]OutputDetail, error) {
	rows, err := q.Query(ctx, `
		SELECT o.vout_index, o.value_satoshis, o.script_pubkey, o.script_type,
		       o.witness_version, o.witness_program, oa.address, u.spent
		FROM transaction_outputs o
		LEFT JOIN output_addresses oa ON oa.txid = o.txid AND oa.vout_index = o.vout_index
		LEFT JOIN utxo_state u ON u.txid = o.txid AND u.vout_index = o.vout_index
		WHERE o.txid = $1
		ORDER BY o.vout_index
	`, txid)
	if err != nil {
		return nil, fmt.Errorf("query: outputs for %s: %w", txid, err)
	}
	defer rows.Close()

	var outputs []OutputDetail
	for rows.Next() {
		var out OutputDetail
		var scriptPubKey, witnessProgram []byte
		var scriptType string
		if err := rows.Scan(&out.VoutIndex, &out.ValueSatoshis, &scriptPubKey, &scriptType,
			&out.WitnessVersion, &witnessProgram, &out.Address, &out.Spent); err != nil {
			return nil, fmt.Errorf("query: outputs for %s: scan: %w", txid, err)
		}
		out.ScriptPubKeyHex = hex.EncodeToString(scriptPubKey)
		out.ScriptType = scriptType
		out.ValueQOGE = chain.Amount(out.ValueSatoshis).String()
		if witnessProgram != nil {
			h := hex.EncodeToString(witnessProgram)
			out.WitnessProgram = &h
		}
		outputs = append(outputs, out)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: outputs for %s: %w", txid, err)
	}

	if err := attachParticipants(ctx, q, txid, outputs); err != nil {
		return nil, err
	}
	return outputs, nil
}

// attachParticipants fills in Participants for any multisig outputs in
// outputs, in place. Participant addresses are search/display identities
// only (output_participants) — never merged into Address, which remains
// the sole balance-accounting destination field (docs/ARCHITECTURE.md §7).
func attachParticipants(ctx context.Context, q querier, txid string, outputs []OutputDetail) error {
	byVout := make(map[int]*OutputDetail, len(outputs))
	for i := range outputs {
		if outputs[i].ScriptType == "multisig" {
			byVout[outputs[i].VoutIndex] = &outputs[i]
		}
	}
	if len(byVout) == 0 {
		return nil
	}

	rows, err := q.Query(ctx, `
		SELECT vout_index, address FROM output_participants WHERE txid = $1 ORDER BY vout_index, address
	`, txid)
	if err != nil {
		return fmt.Errorf("query: participants for %s: %w", txid, err)
	}
	defer rows.Close()

	for rows.Next() {
		var voutIndex int
		var addr string
		if err := rows.Scan(&voutIndex, &addr); err != nil {
			return fmt.Errorf("query: participants for %s: scan: %w", txid, err)
		}
		if out, ok := byVout[voutIndex]; ok {
			out.Participants = append(out.Participants, addr)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("query: participants for %s: %w", txid, err)
	}
	return nil
}
