package rpc

import "context"

// BlockchainInfo mirrors the fields of Core's getblockchaininfo response
// that the explorer currently cares about. Core returns more fields than
// this; unused ones are simply not decoded.
type BlockchainInfo struct {
	Chain                string  `json:"chain"`
	Blocks               int64   `json:"blocks"`
	Headers              int64   `json:"headers"`
	BestBlockHash        string  `json:"bestblockhash"`
	Difficulty           float64 `json:"difficulty"`
	VerificationProgress float64 `json:"verificationprogress"`
	InitialBlockDownload bool    `json:"initialblockdownload"`
	Pruned               bool    `json:"pruned"`
}

// NetworkInfo mirrors the fields of Core's getnetworkinfo response that the
// explorer currently cares about.
type NetworkInfo struct {
	Version         int    `json:"version"`
	SubVersion      string `json:"subversion"`
	ProtocolVersion int    `json:"protocolversion"`
	Connections     int    `json:"connections"`
}

// TxIndexStatus mirrors one entry of Core's getindexinfo response.
type TxIndexStatus struct {
	Synced          bool  `json:"synced"`
	BestBlockHeight int64 `json:"best_block_height"`
}

// IndexInfo mirrors Core's getindexinfo response. Only txindex is
// requested/used today; other indexes (if any) are ignored.
type IndexInfo struct {
	TxIndex *TxIndexStatus `json:"txindex"`
}

// GetBlockchainInfo calls getblockchaininfo.
func (c *Client) GetBlockchainInfo(ctx context.Context) (BlockchainInfo, error) {
	var info BlockchainInfo
	err := c.CallInto(ctx, &info, "getblockchaininfo")
	return info, err
}

// GetNetworkInfo calls getnetworkinfo.
func (c *Client) GetNetworkInfo(ctx context.Context) (NetworkInfo, error) {
	var info NetworkInfo
	err := c.CallInto(ctx, &info, "getnetworkinfo")
	return info, err
}

// GetIndexInfo calls getindexinfo, requesting only the txindex entry.
func (c *Client) GetIndexInfo(ctx context.Context) (IndexInfo, error) {
	var info IndexInfo
	err := c.CallInto(ctx, &info, "getindexinfo", "txindex")
	return info, err
}

// GetBestBlockHash calls getbestblockhash.
func (c *Client) GetBestBlockHash(ctx context.Context) (string, error) {
	var hash string
	err := c.CallInto(ctx, &hash, "getbestblockhash")
	return hash, err
}

// GetBlockCount calls getblockcount, returning Core's current best-chain
// height.
func (c *Client) GetBlockCount(ctx context.Context) (int64, error) {
	var height int64
	err := c.CallInto(ctx, &height, "getblockcount")
	return height, err
}
