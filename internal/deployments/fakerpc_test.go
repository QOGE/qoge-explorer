package deployments

import (
	"context"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/store"
)

// fakeRPCClient is a deterministic RPCClient double for race-fixture
// tests (spec item 32) — no sleep, no real network I/O, no running
// qogecoind. blockCountSeq/blockHashSeq are consumed one call at a time;
// once only one value remains it is returned repeatedly, mirroring
// internal/mempool/fakerpc_test.go's fakeRPCClient exactly (duplicated —
// test-only files aren't importable across packages).
type fakeRPCClient struct {
	blockCountSeq []int64
	blockHashSeq  []string

	deploymentInfo    rpc.RawDeploymentInfo
	deploymentInfoErr error
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

func (f *fakeRPCClient) GetBlockHash(_ context.Context, _ int64) (string, error) {
	if len(f.blockHashSeq) == 0 {
		return "", fmt.Errorf("fakeRPCClient: no GetBlockHash value queued")
	}
	v := f.blockHashSeq[0]
	if len(f.blockHashSeq) > 1 {
		f.blockHashSeq = f.blockHashSeq[1:]
	}
	return v, nil
}

func (f *fakeRPCClient) GetDeploymentInfo(_ context.Context, _ string) (rpc.RawDeploymentInfo, error) {
	if f.deploymentInfoErr != nil {
		return rpc.RawDeploymentInfo{}, f.deploymentInfoErr
	}
	return f.deploymentInfo, nil
}

var _ RPCClient = (*fakeRPCClient)(nil)

// fakeConfirmedTip is a deterministic ConfirmedTipReader double, with the
// same "last value repeats" queue-consumption behavior as
// fakeRPCClient's sequences above.
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
