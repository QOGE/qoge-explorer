package indexer

import (
	"context"

	"github.com/QOGE/qoge-explorer/internal/rpc"
)

// RPCClient is the narrow set of Core RPC operations the indexer needs.
// *rpc.Client satisfies it in production; indexer tests supply a
// deterministic fake instead of requiring a running qogecoind.
type RPCClient interface {
	GetBlockCount(ctx context.Context) (int64, error)
	GetBlockHash(ctx context.Context, height int64) (string, error)
	GetBlockVerbose2(ctx context.Context, hash string) (rpc.RawBlock, error)
}

var _ RPCClient = (*rpc.Client)(nil)
