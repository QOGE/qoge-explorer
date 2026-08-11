package query

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/jackc/pgx/v5"
)

// ErrMempoolGenerationChanged means a caller-supplied MempoolCursor was
// minted against a mempool_state.generation that no longer matches the
// current snapshot's generation. Because internal/mempool.Store.ReplaceSnapshot
// always fully replaces mempool_transactions (never an additive delta — see
// docs/ARCHITECTURE.md §22), a cursor's (entry_time, txid) pair only has
// meaning relative to the exact generation it was read from: silently
// continuing pagination against a newer generation could skip rows,
// duplicate rows, or (after a full replacement) walk an entirely different
// transaction set while looking like a normal "next page". internal/api
// maps this to HTTP 409; internal/web resets cleanly to the first page —
// see docs/ARCHITECTURE.md §23.
var ErrMempoolGenerationChanged = errors.New("query: mempool generation changed")

// MempoolState is a read-only view of the mempool_state('main') singleton
// row PLUS, read from the SAME PostgreSQL snapshot, the confirmed chain's
// own indexed checkpoint (sync_state) — never two independent queries. The
// two are compared to derive Status/Stale (see docs/ARCHITECTURE.md §23
// "Freshness: comparing two independently-advancing checkpoints"):
//
//   - UNINITIALIZED (Initialized=false): internal/mempool has never
//     successfully published a snapshot. Status is "uninitialized", Stale
//     is always false — never true, precisely so a reader cannot mistake
//     "never synchronized" for "synchronized but out of date". This is a
//     third, explicit state, not a degenerate case of the other two.
//   - FRESH (Initialized=true, anchor == confirmed tip): the cached
//     mempool rows were observed against the exact confirmed chain state a
//     reader is also seeing right now.
//   - STALE (Initialized=true, anchor != confirmed tip): the cached
//     mempool rows are real, previously-observed mempool state, but
//     confirmed indexing has since advanced past the tip the snapshot was
//     anchored to. This is normal asynchronous operation, NOT corruption —
//     see internal/mempool/sync.go's anchor-acquisition comments — but a
//     reader must never be left to assume a stale snapshot is current.
type MempoolState struct {
	Initialized bool  `json:"initialized"`
	Generation  int64 `json:"generation"`

	CoreTipHeight *int64  `json:"core_tip_height,omitempty"`
	CoreTipHash   *string `json:"core_tip_hash,omitempty"`

	TxCount          int    `json:"tx_count"`
	TotalVSize       int64  `json:"total_vsize"`
	TotalFeeSatoshis int64  `json:"total_fee_satoshis"`
	TotalFeeQOGE     string `json:"total_fee_qoge"`

	ObservedAt *time.Time `json:"observed_at,omitempty"`

	// ConfirmedIndexedHeight/ConfirmedIndexedHash mirror Status (this
	// package's confirmed-chain checkpoint), read from the SAME snapshot
	// as the mempool_state fields above — see statusFrom.
	ConfirmedIndexedHeight int64   `json:"confirmed_indexed_height"`
	ConfirmedIndexedHash   *string `json:"confirmed_indexed_hash,omitempty"`

	// Status is one of "uninitialized", "fresh", "stale" — see the type's
	// doc comment. Stale is the same fact as a plain bool, kept alongside
	// Status for callers that only care about the fresh/not-fresh
	// distinction; Stale is always false when Initialized is false.
	Status string `json:"status"`
	Stale  bool   `json:"stale"`
}

// mempoolStateFrom reads mempool_state('main') and the confirmed sync_state
// checkpoint against q, and derives Status/Stale by comparing them —
// callers needing this alongside other mempool reads (MempoolOverview,
// mempoolTransactionDetail) MUST call it against an already-open read-only
// REPEATABLE READ transaction (see readTx), never against s.pool directly,
// so the comparison and every other read in the same response share one
// consistent snapshot (spec: docs/ARCHITECTURE.md §23 "Repeatable-read
// snapshots").
func mempoolStateFrom(ctx context.Context, q querier) (MempoolState, error) {
	var st MempoolState
	var observedAt *time.Time
	err := q.QueryRow(ctx, `
		SELECT initialized, generation, core_tip_height, core_tip_hash,
		       tx_count, total_vsize, total_fee_satoshis, observed_at
		FROM mempool_state WHERE name = 'main'
	`).Scan(&st.Initialized, &st.Generation, &st.CoreTipHeight, &st.CoreTipHash,
		&st.TxCount, &st.TotalVSize, &st.TotalFeeSatoshis, &observedAt)
	if err != nil {
		return MempoolState{}, fmt.Errorf("query: mempool state: %w", err)
	}
	st.ObservedAt = observedAt
	st.TotalFeeQOGE = chain.Amount(st.TotalFeeSatoshis).String()

	confirmed, err := statusFrom(ctx, q)
	if err != nil {
		return MempoolState{}, err
	}
	st.ConfirmedIndexedHeight = confirmed.IndexedHeight
	st.ConfirmedIndexedHash = confirmed.IndexedHash

	switch {
	case !st.Initialized:
		st.Status = "uninitialized"
		st.Stale = false
	case st.CoreTipHeight != nil && st.CoreTipHash != nil && confirmed.IndexedHash != nil &&
		*st.CoreTipHeight == confirmed.IndexedHeight && *st.CoreTipHash == *confirmed.IndexedHash:
		st.Status = "fresh"
		st.Stale = false
	default:
		st.Status = "stale"
		st.Stale = true
	}

	return st, nil
}

// MempoolState reads the current mempool cache state and its freshness
// relative to the confirmed indexed tip, from one read-only REPEATABLE READ
// snapshot (two statements — mempool_state and sync_state — that must never
// be read as two independent, potentially-inconsistent queries; see
// mempoolStateFrom).
func (s *Store) MempoolState(ctx context.Context) (MempoolState, error) {
	tx, done, err := s.readTx(ctx)
	if err != nil {
		return MempoolState{}, err
	}
	defer done()
	return mempoolStateFrom(ctx, tx)
}

// MempoolCursor is a generation-safe keyset pagination cursor for the
// mempool transaction list (ordered entry_time DESC, txid DESC — see
// mempoolTxPageFrom). Generation must match the CURRENT mempool_state
// generation for the cursor to be honored — see ErrMempoolGenerationChanged.
type MempoolCursor struct {
	Generation int64  `json:"generation"`
	EntryTime  int64  `json:"before_entry_time"`
	TxID       string `json:"before_txid"`
}

// MempoolTxSummary is one cached mempool transaction as shown in a list —
// deliberately body-only: no witness stack, no input/output accounting (see
// docs/ARCHITECTURE.md §23 "List pages never load witness or perform
// accounting"). VSize is the transaction's own BIP141 vsize as decoded from
// Core's getrawtransaction, frozen by Phase 2F.1 — never Core's
// sigop-adjusted mempool policy vsize (internal/mempool's
// RawMempoolEntry.VSize is not even persisted; see
// docs/ARCHITECTURE.md §22).
type MempoolTxSummary struct {
	TxID    string `json:"txid"`
	WTxID   string `json:"wtxid"`
	Version uint32 `json:"version"`
	Size    int    `json:"size"`
	VSize   int    `json:"vsize"`
	Weight  int    `json:"weight"`

	FeeSatoshis int64  `json:"fee_satoshis"`
	FeeQOGE     string `json:"fee_qoge"`
	EntryTime   int64  `json:"entry_time"`
	EntryHeight *int64 `json:"entry_height,omitempty"`
	Replaceable *bool  `json:"replaceable,omitempty"`
}

// MempoolTxPage is one bounded, generation-safe keyset-paginated page of the
// current mempool snapshot's transactions, newest-entry-first.
type MempoolTxPage struct {
	Transactions []MempoolTxSummary `json:"transactions"`
	Generation   int64              `json:"generation"`
	// NextCursor, when non-nil, is the cursor a caller should pass back in
	// to fetch the next page of THIS SAME generation. A caller that waits
	// too long and reuses it after a ReplaceSnapshot has advanced the
	// generation gets ErrMempoolGenerationChanged instead of silently
	// mixed results.
	NextCursor *MempoolCursor `json:"next_cursor,omitempty"`
}

// mempoolTxPageFrom lists the current snapshot's transactions ordered
// entry_time DESC, txid DESC (spec: deterministic, never relying on
// PostgreSQL's physical row order), keyset-paginated by
// (cursor.EntryTime, cursor.TxID). generation is the CURRENT
// mempool_state.generation (already validated by the caller against
// cursor.Generation — see MempoolOverview) and is only threaded through so
// the returned page's own Generation/NextCursor are self-consistent.
func mempoolTxPageFrom(ctx context.Context, q querier, generation int64, cursor *MempoolCursor, pageSize int) (MempoolTxPage, error) {
	pageSize = clampPageSize(pageSize)

	var beforeEntryTime *int64
	var beforeTxID *string
	if cursor != nil {
		beforeEntryTime = &cursor.EntryTime
		beforeTxID = &cursor.TxID
	}

	rows, err := q.Query(ctx, `
		SELECT txid, wtxid, version, size, vsize, weight, fee_satoshis, entry_time, entry_height, replaceable
		FROM mempool_transactions
		WHERE $1::bigint IS NULL OR entry_time < $1 OR (entry_time = $1 AND txid < $2)
		ORDER BY entry_time DESC, txid DESC
		LIMIT $3
	`, beforeEntryTime, beforeTxID, pageSize)
	if err != nil {
		return MempoolTxPage{}, fmt.Errorf("query: mempool transactions: %w", err)
	}
	defer rows.Close()

	page := MempoolTxPage{Generation: generation}
	for rows.Next() {
		var t MempoolTxSummary
		var version int64
		if err := rows.Scan(&t.TxID, &t.WTxID, &version, &t.Size, &t.VSize, &t.Weight,
			&t.FeeSatoshis, &t.EntryTime, &t.EntryHeight, &t.Replaceable); err != nil {
			return MempoolTxPage{}, fmt.Errorf("query: mempool transactions: scan: %w", err)
		}
		t.Version = uint32(version)
		t.FeeQOGE = chain.Amount(t.FeeSatoshis).String()
		page.Transactions = append(page.Transactions, t)
	}
	if err := rows.Err(); err != nil {
		return MempoolTxPage{}, fmt.Errorf("query: mempool transactions: %w", err)
	}

	if len(page.Transactions) == pageSize {
		last := page.Transactions[len(page.Transactions)-1]
		page.NextCursor = &MempoolCursor{Generation: generation, EntryTime: last.EntryTime, TxID: last.TxID}
	}
	return page, nil
}

// MempoolOverview is the /mempool page's and GET /api/v1/mempool's state +
// transaction-page pair, read from ONE read-only REPEATABLE READ snapshot —
// the same composite-read-snapshot property ExplorerOverview/AddressDetail
// already give the confirmed-chain pages (see composite.go). Without this,
// a concurrent ReplaceSnapshot committing between an independent state read
// and an independent list read could report a generation that doesn't
// match the transactions actually shown.
type MempoolOverview struct {
	State        MempoolState  `json:"state"`
	Transactions MempoolTxPage `json:"transactions"`
}

// MempoolOverview reads MempoolState and one page of the current mempool
// transaction list from one snapshot. cursor, if non-nil, must have been
// returned by a previous call (via MempoolTxPage.NextCursor) — if its
// Generation no longer matches the CURRENT mempool_state.generation (read
// in this same snapshot), the whole call fails with
// ErrMempoolGenerationChanged rather than silently paginating against a
// mismatched snapshot (docs/ARCHITECTURE.md §23 "Generation-safe
// pagination").
func (s *Store) MempoolOverview(ctx context.Context, cursor *MempoolCursor, pageSize int) (MempoolOverview, error) {
	tx, done, err := s.readTx(ctx)
	if err != nil {
		return MempoolOverview{}, err
	}
	defer done()

	state, err := mempoolStateFrom(ctx, tx)
	if err != nil {
		return MempoolOverview{}, err
	}
	fireSnapshotTestHook() // snapshot is now fixed as of this first read

	if cursor != nil && cursor.Generation != state.Generation {
		return MempoolOverview{}, ErrMempoolGenerationChanged
	}

	page, err := mempoolTxPageFrom(ctx, tx, state.Generation, cursor, pageSize)
	if err != nil {
		return MempoolOverview{}, err
	}

	return MempoolOverview{State: state, Transactions: page}, nil
}

// MempoolInputDetail is one mempool transaction input (vin). A mempool
// transaction can never be coinbase-shaped (internal/mempool's Candidate.
// validate rejects it before it ever reaches PostgreSQL — see
// migrations/0002_mempool_cache.up.sql's mempool_inputs NOT NULL prevout
// columns), so unlike query.InputDetail there is no CoinbaseHex field at
// all.
type MempoolInputDetail struct {
	VinIndex      int    `json:"vin_index"`
	PrevTxID      string `json:"prev_txid"`
	PrevVoutIndex int    `json:"prev_vout_index"`
	ScriptSigHex  string `json:"script_sig_hex"`
	Sequence      uint32 `json:"sequence"`
	// Witness reuses query.WitnessItem exactly — same default-hidden,
	// explicit-opt-in raw-data policy as confirmed transaction detail (see
	// docs/ARCHITECTURE.md §23 "Default witness omission").
	Witness []WitnessItem `json:"witness,omitempty"`
}

// MempoolOutputDetail is one mempool transaction output (vout). Address is
// the sole balance-accounting destination field; Participants (bare
// multisig co-signer identities) are kept structurally separate, exactly
// mirroring query.OutputDetail's confirmed-chain distinction — see
// docs/ARCHITECTURE.md §7/§13.A and §23. There is no Spent field: a mempool
// output has no UTXO concept at all.
type MempoolOutputDetail struct {
	VoutIndex       int      `json:"vout_index"`
	ValueSatoshis   int64    `json:"value_sats"`
	ValueQOGE       string   `json:"value_qoge"`
	ScriptPubKeyHex string   `json:"script_pubkey_hex"`
	ScriptType      string   `json:"script_type"`
	Address         *string  `json:"address,omitempty"`
	WitnessVersion  *int     `json:"witness_version,omitempty"`
	WitnessProgram  *string  `json:"witness_program,omitempty"`
	Participants    []string `json:"participants,omitempty"`
}

// MempoolTransactionDetail is one cached mempool transaction's full body —
// deliberately NOT including any derived input-value/pending-balance
// accounting (docs/ARCHITECTURE.md §23 "No derived input accounting"): Fee
// is displayed exactly as Core reported it (FeeSatoshis, persisted in
// Phase 2F.1 via exact decode.DecodeAmount parsing — never reconstructed by
// summing prevout values here).
type MempoolTransactionDetail struct {
	TxID     string `json:"txid"`
	WTxID    string `json:"wtxid"`
	Version  uint32 `json:"version"`
	LockTime uint32 `json:"locktime"`
	Size     int    `json:"size"`
	VSize    int    `json:"vsize"`
	Weight   int    `json:"weight"`

	FeeSatoshis int64  `json:"fee_satoshis"`
	FeeQOGE     string `json:"fee_qoge"`
	EntryTime   int64  `json:"entry_time"`
	EntryHeight *int64 `json:"entry_height,omitempty"`
	Replaceable *bool  `json:"replaceable,omitempty"`

	// Depends lists this transaction's in-mempool parent txids, in
	// deterministic (lexicographic) order — never the physical row order
	// internal/mempool happened to insert them in.
	Depends []string `json:"depends"`

	Inputs  []MempoolInputDetail  `json:"inputs"`
	Outputs []MempoolOutputDetail `json:"outputs"`

	// Snapshot is the mempool_state/anchor/generation associated with THIS
	// read, from the SAME snapshot the transaction body itself was read
	// from — so a caller can tell whether the transaction it's looking at
	// came from a fresh or stale cache without a second round-trip.
	Snapshot MempoolState `json:"snapshot"`
}

// MempoolTransactionByTxID looks up a currently-cached mempool transaction
// by its non-witness identity (txid). Every read that makes up the
// response — the transaction body, its dependencies, inputs, outputs, and
// the associated MempoolState — comes from ONE read-only REPEATABLE READ
// snapshot, so a concurrent ReplaceSnapshot can never produce a response
// mixing rows from two different generations (docs/ARCHITECTURE.md §23
// "Repeatable-read snapshots").
func (s *Store) MempoolTransactionByTxID(ctx context.Context, txid string, includeRawWitness bool) (MempoolTransactionDetail, error) {
	tx, done, err := s.readTx(ctx)
	if err != nil {
		return MempoolTransactionDetail{}, err
	}
	defer done()
	return mempoolTransactionDetail(ctx, tx, txid, includeRawWitness)
}

// MempoolTransactionByWTxID looks up a currently-cached mempool transaction
// by its witness identity (wtxid). Unlike the confirmed-chain
// TransactionByWTxID, a mempool transaction has exactly one observed
// witness serialization at a time (mempool_transactions.wtxid is UNIQUE,
// not a one-of-many variant — see migrations/0002_mempool_cache.up.sql), so
// there is no "which variant" ambiguity to resolve: the wtxid->txid lookup
// and every subsequent detail read still share the SAME read-only
// REPEATABLE READ snapshot, exactly mirroring the confirmed-chain pattern's
// reasoning.
func (s *Store) MempoolTransactionByWTxID(ctx context.Context, wtxid string, includeRawWitness bool) (MempoolTransactionDetail, error) {
	tx, done, err := s.readTx(ctx)
	if err != nil {
		return MempoolTransactionDetail{}, err
	}
	defer done()

	var txid string
	err = tx.QueryRow(ctx, `SELECT txid FROM mempool_transactions WHERE wtxid = $1`, wtxid).Scan(&txid)
	if errors.Is(err, pgx.ErrNoRows) {
		return MempoolTransactionDetail{}, ErrNotFound
	}
	if err != nil {
		return MempoolTransactionDetail{}, fmt.Errorf("query: mempool transaction wtxid %s: %w", wtxid, err)
	}
	return mempoolTransactionDetail(ctx, tx, txid, includeRawWitness)
}

// mempoolTransactionDetail builds a MempoolTransactionDetail for txid using
// q — the caller's read-only snapshot transaction. A missing txid is
// ErrNotFound regardless of whether the mempool is currently initialized —
// the caller never falls back to querying Core, and there is no historical
// mempool data to fall back to by design (full-replacement snapshots only —
// docs/ARCHITECTURE.md §23 "Detail not found").
func mempoolTransactionDetail(ctx context.Context, q querier, txid string, includeRawWitness bool) (MempoolTransactionDetail, error) {
	var d MempoolTransactionDetail
	var version, lockTime int64
	err := q.QueryRow(ctx, `
		SELECT txid, wtxid, version, locktime, size, vsize, weight, fee_satoshis, entry_time, entry_height, replaceable
		FROM mempool_transactions WHERE txid = $1
	`, txid).Scan(&d.TxID, &d.WTxID, &version, &lockTime, &d.Size, &d.VSize, &d.Weight,
		&d.FeeSatoshis, &d.EntryTime, &d.EntryHeight, &d.Replaceable)
	if errors.Is(err, pgx.ErrNoRows) {
		return MempoolTransactionDetail{}, ErrNotFound
	}
	if err != nil {
		return MempoolTransactionDetail{}, fmt.Errorf("query: mempool transaction %s: %w", txid, err)
	}
	fireSnapshotTestHook() // snapshot is now fixed as of this first statement

	d.Version = uint32(version)
	d.LockTime = uint32(lockTime)
	d.FeeQOGE = chain.Amount(d.FeeSatoshis).String()

	state, err := mempoolStateFrom(ctx, q)
	if err != nil {
		return MempoolTransactionDetail{}, err
	}
	d.Snapshot = state

	depends, err := mempoolDependencies(ctx, q, txid)
	if err != nil {
		return MempoolTransactionDetail{}, err
	}
	d.Depends = depends

	witnessByVin, err := mempoolWitnessByVin(ctx, q, txid, includeRawWitness)
	if err != nil {
		return MempoolTransactionDetail{}, err
	}

	inputs, err := mempoolInputs(ctx, q, txid, witnessByVin)
	if err != nil {
		return MempoolTransactionDetail{}, err
	}
	d.Inputs = inputs

	outputs, err := mempoolOutputs(ctx, q, txid)
	if err != nil {
		return MempoolTransactionDetail{}, err
	}
	d.Outputs = outputs

	return d, nil
}

// mempoolDependencies returns txid's in-mempool parent txids in
// deterministic (lexicographic) order.
func mempoolDependencies(ctx context.Context, q querier, txid string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT depends_on_txid FROM mempool_dependencies WHERE txid = $1 ORDER BY depends_on_txid
	`, txid)
	if err != nil {
		return nil, fmt.Errorf("query: mempool dependencies for %s: %w", txid, err)
	}
	defer rows.Close()

	var deps []string
	for rows.Next() {
		var dep string
		if err := rows.Scan(&dep); err != nil {
			return nil, fmt.Errorf("query: mempool dependencies for %s: scan: %w", txid, err)
		}
		deps = append(deps, dep)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: mempool dependencies for %s: %w", txid, err)
	}
	return deps, nil
}

// mempoolWitnessByVin returns txid's witness stack, grouped by vin_index
// and ordered by item_index within each — mirrors transactions.go's
// witnessByVin exactly, reading mempool_input_witness (keyed directly by
// txid, unlike the confirmed schema's per-wtxid-variant table — a mempool
// transaction has only one observed witness serialization at a time). Raw
// bytes are only fetched from PostgreSQL at all when includeRaw is true — a
// P2QPK signature item alone is 17,088 bytes (docs/ARCHITECTURE.md §23
// "Default witness omission").
func mempoolWitnessByVin(ctx context.Context, q querier, txid string, includeRaw bool) (map[int][]WitnessItem, error) {
	var rows pgx.Rows
	var err error
	if includeRaw {
		rows, err = q.Query(ctx, `
			SELECT vin_index, item_index, data FROM mempool_input_witness
			WHERE txid = $1 ORDER BY vin_index, item_index
		`, txid)
	} else {
		rows, err = q.Query(ctx, `
			SELECT vin_index, item_index, octet_length(data) FROM mempool_input_witness
			WHERE txid = $1 ORDER BY vin_index, item_index
		`, txid)
	}
	if err != nil {
		return nil, fmt.Errorf("query: mempool witness for %s: %w", txid, err)
	}
	defer rows.Close()

	result := map[int][]WitnessItem{}
	for rows.Next() {
		var vin, itemIndex int
		var item WitnessItem
		if includeRaw {
			var data []byte
			if err := rows.Scan(&vin, &itemIndex, &data); err != nil {
				return nil, fmt.Errorf("query: mempool witness for %s: scan: %w", txid, err)
			}
			item.SizeBytes = len(data)
			hexData := hex.EncodeToString(data)
			item.DataHex = &hexData
		} else {
			if err := rows.Scan(&vin, &itemIndex, &item.SizeBytes); err != nil {
				return nil, fmt.Errorf("query: mempool witness for %s: scan: %w", txid, err)
			}
		}
		item.ItemIndex = itemIndex
		result[vin] = append(result[vin], item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: mempool witness for %s: %w", txid, err)
	}
	return result, nil
}

func mempoolInputs(ctx context.Context, q querier, txid string, witnessByVin map[int][]WitnessItem) ([]MempoolInputDetail, error) {
	rows, err := q.Query(ctx, `
		SELECT vin_index, prev_txid, prev_vout_index, script_sig, sequence
		FROM mempool_inputs WHERE txid = $1 ORDER BY vin_index
	`, txid)
	if err != nil {
		return nil, fmt.Errorf("query: mempool inputs for %s: %w", txid, err)
	}
	defer rows.Close()

	var inputs []MempoolInputDetail
	for rows.Next() {
		var in MempoolInputDetail
		var scriptSig []byte
		var sequence int64
		if err := rows.Scan(&in.VinIndex, &in.PrevTxID, &in.PrevVoutIndex, &scriptSig, &sequence); err != nil {
			return nil, fmt.Errorf("query: mempool inputs for %s: scan: %w", txid, err)
		}
		in.ScriptSigHex = hex.EncodeToString(scriptSig)
		in.Sequence = uint32(sequence)
		in.Witness = witnessByVin[in.VinIndex]
		inputs = append(inputs, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: mempool inputs for %s: %w", txid, err)
	}
	return inputs, nil
}

func mempoolOutputs(ctx context.Context, q querier, txid string) ([]MempoolOutputDetail, error) {
	rows, err := q.Query(ctx, `
		SELECT o.vout_index, o.value_satoshis, o.script_pubkey, o.script_type,
		       o.witness_version, o.witness_program, oa.address
		FROM mempool_outputs o
		LEFT JOIN mempool_output_addresses oa ON oa.txid = o.txid AND oa.vout_index = o.vout_index
		WHERE o.txid = $1
		ORDER BY o.vout_index
	`, txid)
	if err != nil {
		return nil, fmt.Errorf("query: mempool outputs for %s: %w", txid, err)
	}
	defer rows.Close()

	var outputs []MempoolOutputDetail
	for rows.Next() {
		var out MempoolOutputDetail
		var scriptPubKey, witnessProgram []byte
		var scriptType string
		if err := rows.Scan(&out.VoutIndex, &out.ValueSatoshis, &scriptPubKey, &scriptType,
			&out.WitnessVersion, &witnessProgram, &out.Address); err != nil {
			return nil, fmt.Errorf("query: mempool outputs for %s: scan: %w", txid, err)
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
		return nil, fmt.Errorf("query: mempool outputs for %s: %w", txid, err)
	}

	if err := attachMempoolParticipants(ctx, q, txid, outputs); err != nil {
		return nil, err
	}
	return outputs, nil
}

// attachMempoolParticipants fills in Participants for any multisig outputs
// in outputs, in place — mirrors transactions.go's attachParticipants
// exactly. Participant addresses are search/display identities only
// (mempool_output_participants) — never merged into Address, which remains
// the sole balance-accounting destination field (docs/ARCHITECTURE.md
// §7/§13.A, extended to the mempool cache in §23).
func attachMempoolParticipants(ctx context.Context, q querier, txid string, outputs []MempoolOutputDetail) error {
	byVout := make(map[int]*MempoolOutputDetail, len(outputs))
	for i := range outputs {
		if outputs[i].ScriptType == "multisig" {
			byVout[outputs[i].VoutIndex] = &outputs[i]
		}
	}
	if len(byVout) == 0 {
		return nil
	}

	rows, err := q.Query(ctx, `
		SELECT vout_index, address FROM mempool_output_participants WHERE txid = $1 ORDER BY vout_index, address
	`, txid)
	if err != nil {
		return fmt.Errorf("query: mempool participants for %s: %w", txid, err)
	}
	defer rows.Close()

	for rows.Next() {
		var voutIndex int
		var addr string
		if err := rows.Scan(&voutIndex, &addr); err != nil {
			return fmt.Errorf("query: mempool participants for %s: scan: %w", txid, err)
		}
		if out, ok := byVout[voutIndex]; ok {
			out.Participants = append(out.Participants, addr)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("query: mempool participants for %s: %w", txid, err)
	}
	return nil
}
