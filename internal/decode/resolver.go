package decode

import (
	"context"
	"fmt"
	"sync"

	"github.com/QOGE/qoge-explorer/internal/rpc"
)

// AddressResolver resolves a raw public key (compressed or uncompressed,
// hex-encoded) to the address Core's own descriptor machinery derives for
// it — the Core-validated fallback the decoder uses for bare P2PK and
// multisig outputs, both of which Core's RPC deliberately omits an address
// field for (docs/ARCHITECTURE.md §7, §9 "P2PK address resolution", §10
// "Bare multisig participant resolution"). An interface, not a concrete
// type, so decoder unit tests can supply a deterministic fake instead of
// requiring a running qogecoind (see decode_test.go's fakeResolver).
type AddressResolver interface {
	ResolvePubKeyAddress(ctx context.Context, pubKeyHex string) (string, error)
}

// CoreAddressResolver resolves pubkey addresses via live Qogecoin Core RPC:
// getdescriptorinfo("pkh(<pubkey>)") for the canonical, checksum-appended
// descriptor, then deriveaddresses(<canonical descriptor>) for the actual
// address. This deliberately never hardcodes QOGE's Base58 version bytes or
// implements address encoding itself — Core remains the sole authority on
// what a pkh(...) descriptor derives to, exactly as it is for every other
// address type the decoder just copies from Core's own RPC output.
//
// Resolved (pubkey -> address) pairs are memoized in-memory for the
// lifetime of the resolver, so decoding many outputs that reuse the same
// early-chain pubkey (e.g. a miner's own P2PK coinbase output repeated
// across many blocks) issues the descriptor RPC round-trip at most once
// per distinct pubkey.
type CoreAddressResolver struct {
	client *rpc.Client

	mu    sync.Mutex
	cache map[string]string
}

// NewCoreAddressResolver wraps an already-configured *rpc.Client.
func NewCoreAddressResolver(client *rpc.Client) *CoreAddressResolver {
	return &CoreAddressResolver{
		client: client,
		cache:  make(map[string]string),
	}
}

// ResolvePubKeyAddress implements AddressResolver. It fails cleanly
// (returns an error) rather than panicking if the resolver was
// constructed with a nil RPC client.
func (r *CoreAddressResolver) ResolvePubKeyAddress(ctx context.Context, pubKeyHex string) (string, error) {
	if r.client == nil {
		return "", fmt.Errorf("resolve address for pubkey %s: CoreAddressResolver has no RPC client (nil)", pubKeyHex)
	}

	r.mu.Lock()
	if addr, ok := r.cache[pubKeyHex]; ok {
		r.mu.Unlock()
		return addr, nil
	}
	r.mu.Unlock()

	descriptor := fmt.Sprintf("pkh(%s)", pubKeyHex)
	info, err := r.client.GetDescriptorInfo(ctx, descriptor)
	if err != nil {
		return "", fmt.Errorf("resolve address for pubkey %s: getdescriptorinfo(%s): %w", pubKeyHex, descriptor, err)
	}
	if info.Descriptor == "" {
		return "", fmt.Errorf("resolve address for pubkey %s: getdescriptorinfo(%s) returned an empty canonical descriptor", pubKeyHex, descriptor)
	}

	addrs, err := r.client.DeriveAddresses(ctx, info.Descriptor)
	if err != nil {
		return "", fmt.Errorf("resolve address for pubkey %s: deriveaddresses(%s): %w", pubKeyHex, info.Descriptor, err)
	}
	if len(addrs) != 1 {
		return "", fmt.Errorf("resolve address for pubkey %s: deriveaddresses(%s) returned %d addresses, want exactly 1", pubKeyHex, info.Descriptor, len(addrs))
	}
	addr := addrs[0]
	if addr == "" {
		return "", fmt.Errorf("resolve address for pubkey %s: deriveaddresses(%s) returned an empty address", pubKeyHex, info.Descriptor)
	}

	r.mu.Lock()
	r.cache[pubKeyHex] = addr
	r.mu.Unlock()
	return addr, nil
}
