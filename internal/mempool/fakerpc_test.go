package mempool

import (
	"context"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/store"
)

// fakeRPCClient is a deterministic RPCClient double for race-fixture tests
// (spec item 39) — no sleep, no real network I/O, no running qogecoind.
// blockCountSeq/bestHashSeq are consumed one call at a time; once only one
// value remains it is returned repeatedly (so a "same tip both times"
// happy-path test needs only a single-element queue, while a "tip moved"
// test supplies two different values).
type fakeRPCClient struct {
	blockCountSeq []int64
	bestHashSeq   []string

	mempoolEntries map[string]rpc.RawMempoolEntry
	mempoolErr     error

	rawTxs    map[string]rpc.RawTransaction
	rawTxErrs map[string]error
}

func (f *fakeRPCClient) GetBlockCount(context.Context) (int64, error) {
	if len(f.blockCountSeq) == 0 {
		return 0, fmt.Errorf("fakeRPCClient: no GetBlockCount value queued")
	}
	v := f.blockCountSeq[0]
	if len(f.blockCountSeq) > 1 {
		f.blockCountSeq = f.blockCountSeq[1:]
	}
	return v, nil
}

func (f *fakeRPCClient) GetBestBlockHash(context.Context) (string, error) {
	if len(f.bestHashSeq) == 0 {
		return "", fmt.Errorf("fakeRPCClient: no GetBestBlockHash value queued")
	}
	v := f.bestHashSeq[0]
	if len(f.bestHashSeq) > 1 {
		f.bestHashSeq = f.bestHashSeq[1:]
	}
	return v, nil
}

func (f *fakeRPCClient) GetRawMempoolVerbose(context.Context) (map[string]rpc.RawMempoolEntry, error) {
	if f.mempoolErr != nil {
		return nil, f.mempoolErr
	}
	return f.mempoolEntries, nil
}

func (f *fakeRPCClient) GetRawTransactionVerbose(_ context.Context, txid string) (rpc.RawTransaction, error) {
	if err, ok := f.rawTxErrs[txid]; ok {
		return rpc.RawTransaction{}, err
	}
	raw, ok := f.rawTxs[txid]
	if !ok {
		return rpc.RawTransaction{}, fmt.Errorf("fakeRPCClient: no raw transaction fixture for %s", txid)
	}
	return raw, nil
}

var _ RPCClient = (*fakeRPCClient)(nil)

// fakeConfirmedTip is a deterministic ConfirmedTipReader double, with the
// same "last value repeats" queue-consumption behavior as fakeRPCClient's
// sequences above.
type fakeConfirmedTip struct {
	seq []store.Checkpoint
	err error
}

func (f *fakeConfirmedTip) Tip(context.Context) (store.Checkpoint, error) {
	if f.err != nil {
		return store.Checkpoint{}, f.err
	}
	if len(f.seq) == 0 {
		return store.Checkpoint{}, fmt.Errorf("fakeConfirmedTip: no Checkpoint value queued")
	}
	v := f.seq[0]
	if len(f.seq) > 1 {
		f.seq = f.seq[1:]
	}
	return v, nil
}

var _ ConfirmedTipReader = (*fakeConfirmedTip)(nil)
