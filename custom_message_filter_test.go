package lnd

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// newTestPubkey returns a deterministic compressed public key and its raw
// serialization for the given seed byte.
func newTestPubkey(t *testing.T, seed byte) ([33]byte, []byte) {
	t.Helper()

	var key [32]byte
	key[31] = seed

	priv, _ := btcec.PrivKeyFromBytes(key[:])
	compressed := priv.PubKey().SerializeCompressed()

	var peer [33]byte
	copy(peer[:], compressed)

	return peer, compressed
}

// TestNewCustomMessageFilterEmpty asserts that no conditions results in an
// empty filter that does not restrict anything.
func TestNewCustomMessageFilterEmpty(t *testing.T) {
	filter, err := newCustomMessageFilter(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !filter.empty() {
		t.Fatalf("expected empty filter")
	}

	var nilFilter *customMessageFilter
	if !nilFilter.empty() {
		t.Fatalf("expected nil filter to be empty")
	}
}

// TestNewCustomMessageFilterPubkeys verifies that peer conditions require a
// valid 33-byte compressed public key, deduplicate identical entries, and do
// not require the peer to be connected.
func TestNewCustomMessageFilterPubkeys(t *testing.T) {
	peer1, raw1 := newTestPubkey(t, 1)
	peer2, raw2 := newTestPubkey(t, 2)

	// The same peer repeated in both compressed and re-parsed form must
	// collapse into a single condition.
	filter, err := newCustomMessageFilter(
		[][]byte{raw1, raw1, raw2},
		[]uint32{uint32(lnwire.CustomTypeStart)},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(filter.peerPubkeys) != 2 {
		t.Fatalf("expected 2 deduplicated peers, got %d",
			len(filter.peerPubkeys))
	}
	if _, ok := filter.peerPubkeys[peer1]; !ok {
		t.Fatalf("peer1 missing from filter")
	}
	if _, ok := filter.peerPubkeys[peer2]; !ok {
		t.Fatalf("peer2 missing from filter")
	}

	// A not-currently-connected peer is accepted without error.
	peer3, raw3 := newTestPubkey(t, 3)
	filter, err = newCustomMessageFilter([][]byte{raw3}, nil)
	if err != nil {
		t.Fatalf("unexpected error for disconnected peer: %v", err)
	}
	if _, ok := filter.peerPubkeys[peer3]; !ok {
		t.Fatalf("peer3 missing from filter")
	}

	privBytes := [32]byte{4: 1}
	priv, _ := btcec.PrivKeyFromBytes(privBytes[:])
	uncompressed := priv.PubKey().SerializeUncompressed()

	invalidPeers := [][]byte{
		nil,
		{},
		make([]byte, 32),
		make([]byte, 34),
		// 65-byte uncompressed keys are not accepted.
		uncompressed,
		// 33 bytes of garbage with an invalid prefix.
		append([]byte{0x00}, make([]byte, 32)...),
	}
	for i, invalid := range invalidPeers {
		if _, err := newCustomMessageFilter(
			[][]byte{invalid}, nil,
		); err == nil {
			t.Fatalf("case %d: expected error for invalid peer", i)
		}
	}
}

// TestNewCustomMessageFilterTypes verifies the accepted type range, the
// handling of enabled protocol overrides, rejection of unknown sub-range
// types, and deduplication.
func TestNewCustomMessageFilterTypes(t *testing.T) {
	// Boundaries of the custom range are valid.
	filter, err := newCustomMessageFilter(nil, []uint32{
		uint32(lnwire.CustomTypeStart),
		65535,
		uint32(lnwire.CustomTypeStart),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(filter.msgTypes) != 2 {
		t.Fatalf("expected 2 deduplicated types, got %d",
			len(filter.msgTypes))
	}

	// A type larger than the 16-bit message type space is rejected.
	if _, err := newCustomMessageFilter(nil, []uint32{65536}); err == nil {
		t.Fatalf("expected error for type > 65535")
	}

	// A type below the custom range is rejected unless overridden.
	const overrideType = lnwire.MessageType(500)
	if lnwire.IsCustomOverride(overrideType) {
		t.Fatalf("type %d unexpectedly overridden", overrideType)
	}
	if _, err := newCustomMessageFilter(
		nil, []uint32{uint32(overrideType)},
	); err == nil {
		t.Fatalf("expected error for non-overridden sub-range type")
	}

	// Enable the override globally (as startup config would) and confirm
	// the filter accepts the type without enabling it itself.
	if err := lnwire.SetCustomOverrides(
		[]uint16{uint16(overrideType)},
	); err != nil {
		t.Fatalf("unable to set override: %v", err)
	}
	t.Cleanup(func() {
		_ = lnwire.SetCustomOverrides(nil)
	})

	filter, err = newCustomMessageFilter(nil,
		[]uint32{uint32(overrideType)})
	if err != nil {
		t.Fatalf("expected overridden type to be accepted: %v", err)
	}
	if _, ok := filter.msgTypes[overrideType]; !ok {
		t.Fatalf("override type missing from filter")
	}
}

// TestCustomMessageFilterMatches covers the OR-within-set / AND-across-sets
// matching semantics.
func TestCustomMessageFilterMatches(t *testing.T) {
	peer1, raw1 := newTestPubkey(t, 1)
	peer2, raw2 := newTestPubkey(t, 2)

	type1 := lnwire.CustomTypeStart + 1
	type2 := lnwire.CustomTypeStart + 2

	msg := func(peer [33]byte, typ lnwire.MessageType) *CustomMessage {
		wireMsg, err := lnwire.NewCustom(typ, []byte{0xaa, 0xbb})
		if err != nil {
			t.Fatalf("unable to create custom message: %v", err)
		}

		return &CustomMessage{Peer: peer, Msg: wireMsg}
	}

	// A nil filter accepts everything.
	var nilFilter *customMessageFilter
	if !nilFilter.matches(msg(peer1, type1)) {
		t.Fatalf("nil filter should accept all messages")
	}

	// Peer-only filter: OR within the peer set.
	peerFilter, err := newCustomMessageFilter(
		[][]byte{raw1, raw2}, nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !peerFilter.matches(msg(peer1, type1)) {
		t.Fatalf("expected message from listed peer to match")
	}
	peer3, _ := newTestPubkey(t, 3)
	if peerFilter.matches(msg(peer3, type1)) {
		t.Fatalf("expected message from unlisted peer to be dropped")
	}

	// Type-only filter: OR within the type set.
	typeFilter, err := newCustomMessageFilter(nil, []uint32{
		uint32(type1), uint32(type2),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !typeFilter.matches(msg(peer3, type2)) {
		t.Fatalf("expected listed type to match from any peer")
	}
	if typeFilter.matches(msg(peer1, type2+1)) {
		t.Fatalf("expected unlisted type to be dropped")
	}

	// Both sets present: both must match.
	bothFilter, err := newCustomMessageFilter(
		[][]byte{raw1}, []uint32{uint32(type1)},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bothFilter.matches(msg(peer1, type1)) {
		t.Fatalf("expected matching peer and type to pass")
	}
	if bothFilter.matches(msg(peer1, type2)) {
		t.Fatalf("expected type mismatch to be dropped")
	}
	if bothFilter.matches(msg(peer2, type1)) {
		t.Fatalf("expected peer mismatch to be dropped")
	}
}
