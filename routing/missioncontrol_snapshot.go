package routing

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"time"
)

const (
	// MinHistoryPageSize is the minimum page size that may be requested for
	// a paginated QueryMissionControl export.
	MinHistoryPageSize = 1

	// MaxHistoryPageSize is the maximum page size that may be requested for
	// a paginated QueryMissionControl export.
	MaxHistoryPageSize = 1000

	// HistorySnapshotTTL is the time for which a paginated mission control
	// history snapshot is retained after it has been created.
	HistorySnapshotTTL = 5 * time.Minute

	// maxHistorySnapshots is the maximum number of history snapshots that
	// may be retained at the same time. Requests that would create a
	// snapshot while the capacity is exhausted are rejected.
	maxHistorySnapshots = 64

	// pageTokenVersion is the version of the opaque page token format.
	pageTokenVersion byte = 1

	// snapshotIDLen is the length of the random snapshot identifier in bytes.
	snapshotIDLen = 16

	// hmacLen is the length of the token's HMAC-SHA256 authenticator.
	hmacLen = sha256.Size

	// pageTokenPayloadLen is the length of the unsigned token payload:
	// version (1) || snapshot ID (16) || offset (4) || expiry (8).
	pageTokenPayloadLen = 1 + snapshotIDLen + 4 + 8

	// pageTokenLen is the total length of an opaque page token: payload
	// followed by its HMAC-SHA256 authenticator.
	pageTokenLen = pageTokenPayloadLen + hmacLen
)

var (
	// ErrPageSizeOutOfRange is returned if a page size is requested that
	// is not between MinHistoryPageSize and MaxHistoryPageSize.
	ErrPageSizeOutOfRange = errors.New("page size must be between " +
		"1 and 1000")

	// ErrPageSizeMismatch is returned if a continuation request specifies
	// a page size that differs from the one used when the snapshot was
	// created.
	ErrPageSizeMismatch = errors.New("page size of a continuation request " +
		"must match the page size of the initial request")

	// ErrInvalidPageToken is returned if a page token cannot be parsed, is
	// of an unsupported version, or fails authentication. This covers
	// malformed, forged, foreign-node and pre-restart tokens.
	ErrInvalidPageToken = errors.New("invalid mission control history " +
		"page token")

	// ErrHistorySnapshotNotFound is returned if the snapshot referenced by
	// a validly signed page token is not known to this mission control
	// instance.
	ErrHistorySnapshotNotFound = errors.New("mission control history " +
		"snapshot not found")

	// ErrHistorySnapshotExpired is returned if the snapshot referenced by
	// a page token has passed its retention time.
	ErrHistorySnapshotExpired = errors.New("mission control history " +
		"snapshot has expired, please start a new export")

	// ErrHistorySnapshotLimitExceeded is returned if a new snapshot cannot
	// be created because the maximum number of concurrently retained
	// snapshots has been reached.
	ErrHistorySnapshotLimitExceeded = errors.New("maximum number of " +
		"concurrent mission control history snapshots reached, " +
		"please retry later")
)

// MissionControlHistoryPage is a single page of a fixed, paginated mission
// control history snapshot.
type MissionControlHistoryPage struct {
	// Pairs contains the node pair histories of this page, ordered by the
	// raw node_from and node_to public key bytes.
	Pairs []MissionControlPairSnapshot

	// NextPageToken is the opaque continuation token that is used to fetch
	// the following page of the same snapshot. It is empty on the last
	// page.
	NextPageToken []byte
}

// snapshotID uniquely identifies a history snapshot for the lifetime of a
// mission control instance.
type snapshotID [snapshotIDLen]byte

// historySnapshot is a fixed copy of the mission control pair history taken
// at a single point in time. Subsequent payment results, imports or history
// resets do not mutate the snapshot.
type historySnapshot struct {
	// id uniquely identifies the snapshot.
	id snapshotID

	// pairs contains all node pairs of the snapshot, sorted by the raw
	// from/to public key bytes.
	pairs []MissionControlPairSnapshot

	// pageSize is the fixed page size that applies to every page of this
	// snapshot.
	pageSize uint32

	// expiresAt is the point in time after which the snapshot may no
	// longer be used.
	expiresAt time.Time
}

// historySnapshotStore retains fixed history snapshots for paginated exports.
// It is not safe for concurrent use on its own; callers must hold the owning
// MissionControl's mutex.
type historySnapshotStore struct {
	// key authenticates page tokens. It is generated randomly when the
	// store is created, so tokens issued by another node or before a
	// restart fail authentication.
	key [hmacLen]byte

	// snapshots contains the currently retained snapshots keyed by their
	// random identifier.
	snapshots map[snapshotID]*historySnapshot
}

// newHistorySnapshotStore creates a new empty snapshot store with a random
// token authentication key.
func newHistorySnapshotStore() (*historySnapshotStore, error) {
	store := &historySnapshotStore{
		snapshots: make(map[snapshotID]*historySnapshot),
	}

	if _, err := rand.Read(store.key[:]); err != nil {
		return nil, err
	}

	return store, nil
}

// create takes a fixed snapshot of the given state, registers it and returns
// it. Expired snapshots are pruned first and creation is rejected if the
// maximum number of concurrently retained snapshots has been reached.
func (s *historySnapshotStore) create(pageSize uint32,
	state *missionControlState, now time.Time) (*historySnapshot, error) {

	s.pruneExpired(now)

	if len(s.snapshots) >= maxHistorySnapshots {
		return nil, ErrHistorySnapshotLimitExceeded
	}

	pairs := make([]MissionControlPairSnapshot, 0, countPairResults(state))
	for fromNode, fromPairs := range state.lastPairResult {
		for toNode, result := range fromPairs {
			pairs = append(pairs, MissionControlPairSnapshot{
				Pair:            NewDirectedNodePair(fromNode, toNode),
				TimedPairResult: result,
			})
		}
	}

	sort.Slice(pairs, func(i, j int) bool {
		fromCmp := bytes.Compare(
			pairs[i].Pair.From[:], pairs[j].Pair.From[:],
		)
		if fromCmp != 0 {
			return fromCmp < 0
		}

		return bytes.Compare(
			pairs[i].Pair.To[:], pairs[j].Pair.To[:],
		) < 0
	})

	var id snapshotID
	for {
		if _, err := rand.Read(id[:]); err != nil {
			return nil, err
		}

		// The collision space is 128 bits, but retry on the
		// astronomically unlikely collision to keep IDs unique.
		if _, ok := s.snapshots[id]; !ok {
			break
		}
	}

	snapshot := &historySnapshot{
		id:        id,
		pairs:     pairs,
		pageSize:  pageSize,
		expiresAt: now.Add(HistorySnapshotTTL),
	}
	s.snapshots[id] = snapshot

	return snapshot, nil
}

// page returns the page of the snapshot starting at the given offset and an
// opaque continuation token for the following page. The next token is empty if
// the returned page is the last one.
func (s *historySnapshotStore) page(snapshot *historySnapshot,
	offset uint32) (*MissionControlHistoryPage, error) {

	if offset > uint32(len(snapshot.pairs)) {
		return nil, ErrInvalidPageToken
	}

	end := offset + snapshot.pageSize
	if end > uint32(len(snapshot.pairs)) {
		end = uint32(len(snapshot.pairs))
	}

	page := &MissionControlHistoryPage{
		Pairs: snapshot.pairs[offset:end],
	}

	if end < uint32(len(snapshot.pairs)) {
		page.NextPageToken = s.signToken(
			snapshot.id, end, snapshot.expiresAt.UnixNano(),
		)
	}

	return page, nil
}

// get parses and authenticates the given page token and returns the page that
// the token refers to. The requested page size may be zero to reuse the
// snapshot's page size; otherwise it must match the snapshot's page size.
func (s *historySnapshotStore) get(token []byte, requestedPageSize uint32,
	now time.Time) (*MissionControlHistoryPage, error) {

	id, offset, expiryNanos, err := s.parseToken(token)
	if err != nil {
		return nil, err
	}

	// A token that has passed its signed expiry time is rejected as
	// expired regardless of whether the snapshot is still present.
	if now.UnixNano() >= expiryNanos {
		s.deleteIfPresent(id)
		return nil, ErrHistorySnapshotExpired
	}

	snapshot, ok := s.snapshots[id]
	if !ok {
		return nil, ErrHistorySnapshotNotFound
	}

	// The snapshot is the authoritative source of the expiry time; make
	// sure the token was in fact issued for this snapshot.
	if snapshot.expiresAt.UnixNano() != expiryNanos {
		return nil, ErrInvalidPageToken
	}

	if requestedPageSize != 0 && requestedPageSize != snapshot.pageSize {
		return nil, ErrPageSizeMismatch
	}

	// All issued offsets are positive multiples of the snapshot's page
	// size; anything else indicates a tampered token.
	if offset%snapshot.pageSize != 0 {
		return nil, ErrInvalidPageToken
	}

	return s.page(snapshot, offset)
}

// pruneExpired removes all snapshots whose retention time has elapsed.
func (s *historySnapshotStore) pruneExpired(now time.Time) {
	for id, snapshot := range s.snapshots {
		if now.UnixNano() >= snapshot.expiresAt.UnixNano() {
			delete(s.snapshots, id)
		}
	}
}

// deleteIfPresent removes the snapshot with the given identifier if it is
// retained.
func (s *historySnapshotStore) deleteIfPresent(id snapshotID) {
	delete(s.snapshots, id)
}

// signToken creates an opaque, authenticated token for the given snapshot
// position. Tokens are deterministic in their contents, so retrying the same
// position yields the same token.
func (s *historySnapshotStore) signToken(id snapshotID, offset uint32,
	expiryNanos int64) []byte {

	payload := make([]byte, pageTokenPayloadLen)
	payload[0] = pageTokenVersion
	copy(payload[1:1+snapshotIDLen], id[:])
	binary.BigEndian.PutUint32(payload[1+snapshotIDLen:], offset)
	binary.BigEndian.PutUint64(
		payload[1+snapshotIDLen+4:], uint64(expiryNanos),
	)

	mac := hmac.New(sha256.New, s.key[:])
	mac.Write(payload)

	return append(payload, mac.Sum(nil)...)
}

// parseToken authenticates and decodes an opaque page token.
func (s *historySnapshotStore) parseToken(token []byte) (snapshotID,
	uint32, int64, error) {

	var id snapshotID

	if len(token) != pageTokenLen {
		return id, 0, 0, ErrInvalidPageToken
	}

	payload := token[:pageTokenPayloadLen]
	receivedMAC := token[pageTokenPayloadLen:]

	mac := hmac.New(sha256.New, s.key[:])
	mac.Write(payload)
	if !hmac.Equal(receivedMAC, mac.Sum(nil)) {
		return id, 0, 0, ErrInvalidPageToken
	}

	if payload[0] != pageTokenVersion {
		return id, 0, 0, ErrInvalidPageToken
	}

	copy(id[:], payload[1:1+snapshotIDLen])
	offset := binary.BigEndian.Uint32(payload[1+snapshotIDLen:])
	expiryNanos := int64(binary.BigEndian.Uint64(
		payload[1+snapshotIDLen+4:],
	))

	return id, offset, expiryNanos, nil
}

// countPairResults returns the total number of tracked node pairs in the
// given mission control state.
func countPairResults(state *missionControlState) int {
	var count int
	for _, fromPairs := range state.lastPairResult {
		count += len(fromPairs)
	}

	return count
}
