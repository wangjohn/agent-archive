package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

const packedSessionIndexDir = "sessions-packed-v1"
const packedSessionIndexThreshold = 4096
const packedSessionIndexShards = 256
const packedSessionIndexMaxBytes = 2 * 1024 * 1024
const packedSessionIndexMaxEntries = 1024

type packedSessionIndex struct {
	Version   int                                   `json:"version"`
	Epoch     string                                `json:"epoch"`
	Revision  string                                `json:"revision"`
	Inventory string                                `json:"inventory"`
	Entries   map[string]qualifiedSessionIndexEntry `json:"entries"`
}

func recoveryMarkerVersion(version int) bool { return version == 1 || version == 2 }
func packedIndexHash(key agentmeta.SessionKey) string {
	sum := sha256.Sum256(key.Encoding())
	return hex.EncodeToString(sum[:])
}
func packedIndexPath(home, shard string) string {
	return filepath.Join(home, packedSessionIndexDir, shard+".idx")
}

func (s *Store) packedSessionIndexEntry(key agentmeta.SessionKey) (qualifiedSessionIndexEntry, bool, error) {
	var marker sessionIndexMarker
	err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker)
	if errors.Is(err, os.ErrNotExist) {
		return qualifiedSessionIndexEntry{}, false, nil
	}
	if err != nil {
		return qualifiedSessionIndexEntry{}, false, ErrSessionIndexRecoveryRequired
	}
	if marker.Version == 1 {
		return qualifiedSessionIndexEntry{}, false, nil
	}
	if marker.Version != 2 || !safeFileComponent(marker.PackedEpoch) || marker.PackedRevision == "" || marker.PackedInventory == "" {
		return qualifiedSessionIndexEntry{}, false, ErrSessionIndexRecoveryRequired
	}
	hash := packedIndexHash(key)
	entry, found, err := s.readPackedEntry(hash, marker)
	if err != nil {
		return qualifiedSessionIndexEntry{}, false, err
	}
	if !found {
		return entry, false, nil
	}
	if err := entry.validate(key); err != nil {
		return entry, false, err
	}
	valid, err := s.matchingRegistration(key, entry.ArchiveSessionID)
	if err != nil {
		return entry, false, err
	}
	if valid {
		entry.Reservation = ""
		return entry, true, nil
	}
	if entry.Reservation == "" {
		return qualifiedSessionIndexEntry{}, false, nil
	}
	// A packed reservation is usable only while its directly referenced durable
	// candidate and parent still prove it. Removing either cannot revive a child.
	candidate, present, err := readJSON[SubagentCandidate](s.subagentCandidatePath(entry.ArchiveSessionID))
	if err != nil || !present {
		return entry, false, ErrSessionIndexRecoveryRequired
	}
	actual, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
	if err != nil || actual != key || candidate.ArchiveSessionID != entry.ArchiveSessionID {
		return entry, false, ErrSessionIndexRecoveryRequired
	}
	parentKey, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.ParentNativeSessionID)
	if err != nil {
		return entry, false, ErrSessionIndexRecoveryRequired
	}
	valid, err = s.matchingRegistration(parentKey, candidate.ParentArchiveSessionID)
	if err != nil || !valid {
		return entry, false, ErrSessionIndexRecoveryRequired
	}
	return entry, true, nil
}

const packedRecordIndexBytes = 76
const packedIndexMagic = "AASIDX01"

type packedIndexHeader struct {
	Version       int    `json:"version"`
	Epoch         string `json:"epoch"`
	Revision      string `json:"revision"`
	Inventory     string `json:"inventory"`
	Count         int    `json:"count"`
	Bytes         int    `json:"bytes"`
	IndexChecksum string `json:"index_checksum"`
	Checksum      string `json:"checksum"`
}

func (header packedIndexHeader) checksum() string {
	header.Checksum = ""
	return phaseFingerprint(header)
}

type packedIndexReader struct {
	file      *os.File
	table     []byte
	dataStart int64
	dataBytes int
	data      []byte
}

func (s *Store) openPackedIndex(shard string, marker sessionIndexMarker) (*packedIndexReader, error) {
	file, err := os.Open(packedIndexPath(s.home, shard))
	if err != nil {
		return nil, ErrSessionIndexRecoveryRequired
	}
	good := false
	defer func() {
		if !good {
			_ = file.Close()
		}
	}()
	var prefix [12]byte
	if _, err := io.ReadFull(file, prefix[:]); err != nil || string(prefix[:8]) != packedIndexMagic {
		return nil, ErrSessionIndexRecoveryRequired
	}
	length := int(binary.BigEndian.Uint32(prefix[8:]))
	if length <= 0 || length > 4096 {
		return nil, ErrSessionIndexRecoveryRequired
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, ErrSessionIndexRecoveryRequired
	}
	var header packedIndexHeader
	if json.Unmarshal(data, &header) != nil || header.Version != 1 || header.Epoch != marker.PackedEpoch || header.Revision != marker.PackedRevision || header.Inventory != marker.PackedInventory || header.Count < 0 || header.Count > packedSessionIndexMaxEntries || header.Bytes < 0 || header.Bytes > packedSessionIndexMaxBytes || header.Checksum == "" || header.Checksum != header.checksum() {
		return nil, ErrSessionIndexRecoveryRequired
	}
	table := make([]byte, header.Count*packedRecordIndexBytes)
	if _, err := io.ReadFull(file, table); err != nil {
		return nil, ErrSessionIndexRecoveryRequired
	}
	sum := sha256.Sum256(table)
	if hex.EncodeToString(sum[:]) != header.IndexChecksum {
		return nil, ErrSessionIndexRecoveryRequired
	}
	start := int64(12 + length + len(table))
	info, err := file.Stat()
	if err != nil || info.Size() != start+int64(header.Bytes) || info.Size() > packedSessionIndexMaxBytes {
		return nil, ErrSessionIndexRecoveryRequired
	}
	shardBytes, decodeErr := hex.DecodeString(shard)
	if decodeErr != nil || len(shardBytes) != 1 {
		return nil, ErrSessionIndexRecoveryRequired
	}
	var offset uint64
	for i := range header.Count {
		row := table[i*packedRecordIndexBytes : (i+1)*packedRecordIndexBytes]
		if row[0] != shardBytes[0] || binary.BigEndian.Uint64(row[32:40]) != offset {
			return nil, ErrSessionIndexRecoveryRequired
		}
		if i > 0 && bytes.Compare(table[(i-1)*packedRecordIndexBytes:(i-1)*packedRecordIndexBytes+32], row[:32]) >= 0 {
			return nil, ErrSessionIndexRecoveryRequired
		}
		size := uint64(binary.BigEndian.Uint32(row[40:44]))
		if size == 0 || size > uint64(header.Bytes) || offset+size > uint64(header.Bytes) {
			return nil, ErrSessionIndexRecoveryRequired
		}
		offset += size
	}
	if offset != uint64(header.Bytes) {
		return nil, ErrSessionIndexRecoveryRequired
	}
	good = true
	return &packedIndexReader{file: file, table: table, dataStart: start, dataBytes: header.Bytes}, nil
}
func (reader *packedIndexReader) entry(index int) (qualifiedSessionIndexEntry, error) {
	row := reader.table[index*packedRecordIndexBytes : (index+1)*packedRecordIndexBytes]
	length := int(binary.BigEndian.Uint32(row[40:44]))
	data := make([]byte, length)

	if reader.data != nil {
		offset := int(binary.BigEndian.Uint64(row[32:40]))
		copy(data, reader.data[offset:offset+length])
	} else if _, err := reader.file.ReadAt(data, reader.dataStart+int64(binary.BigEndian.Uint64(row[32:40]))); err != nil {
		return qualifiedSessionIndexEntry{}, ErrSessionIndexRecoveryRequired
	}

	sum := sha256.Sum256(data)
	if !bytes.Equal(sum[:], row[44:76]) {
		return qualifiedSessionIndexEntry{}, ErrSessionIndexRecoveryRequired
	}
	var entry qualifiedSessionIndexEntry
	if json.Unmarshal(data, &entry) != nil {
		return entry, ErrSessionIndexRecoveryRequired
	}
	key := agentmeta.SessionKey{Agent: entry.Agent, NativeID: entry.NativeID}
	if key.Validate() != nil || packedIndexHash(key) != hex.EncodeToString(row[:32]) || entry.Version != 1 || entry.Recovery || entry.Absent {
		return entry, ErrSessionIndexRecoveryRequired
	}
	if !entry.Conflict && entry.validate(key) != nil {
		return entry, ErrSessionIndexRecoveryRequired
	}
	return entry, nil
}
func (s *Store) readPackedEntry(hash string, marker sessionIndexMarker) (qualifiedSessionIndexEntry, bool, error) {
	reader, err := s.openPackedIndex(hash[:2], marker)
	if err != nil {
		return qualifiedSessionIndexEntry{}, false, err
	}
	defer func() { _ = reader.file.Close() }()
	key, err := hex.DecodeString(hash)
	if err != nil || len(key) != 32 {
		return qualifiedSessionIndexEntry{}, false, ErrSessionIndexRecoveryRequired
	}
	count := len(reader.table) / packedRecordIndexBytes
	index := sort.Search(count, func(i int) bool {
		return bytes.Compare(reader.table[i*packedRecordIndexBytes:i*packedRecordIndexBytes+32], key) >= 0
	})
	if index == count || !bytes.Equal(reader.table[index*packedRecordIndexBytes:index*packedRecordIndexBytes+32], key) {
		return qualifiedSessionIndexEntry{}, false, nil
	}
	entry, err := reader.entry(index)
	return entry, err == nil, err
}
func (s *Store) readPackedSessionIndex(shard string, marker sessionIndexMarker) (packedSessionIndex, error) {
	index := packedSessionIndex{Entries: make(map[string]qualifiedSessionIndexEntry)}
	reader, err := s.openPackedIndex(shard, marker)
	if err != nil {
		return index, err
	}
	defer func() { _ = reader.file.Close() }()
	reader.data, err = io.ReadAll(io.LimitReader(reader.file, int64(reader.dataBytes)+1))
	if err != nil || len(reader.data) != reader.dataBytes {
		return index, ErrSessionIndexRecoveryRequired
	}
	for i := range len(reader.table) / packedRecordIndexBytes {
		entry, err := reader.entry(i)
		if err != nil {
			return index, err
		}
		hash := hex.EncodeToString(reader.table[i*packedRecordIndexBytes : i*packedRecordIndexBytes+32])
		index.Entries[hash] = entry
	}
	return index, nil
}
func encodePackedIndex(index packedSessionIndex) ([]byte, error) {
	keys := make([]string, 0, len(index.Entries))
	for key := range index.Entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	table := make([]byte, len(keys)*packedRecordIndexBytes)
	var data bytes.Buffer
	for i, key := range keys {
		hash, err := hex.DecodeString(key)
		if err != nil || len(hash) != 32 {
			return nil, ErrSessionIndexRecoveryRequired
		}
		record, err := json.Marshal(index.Entries[key])
		if err != nil {
			return nil, err
		}
		row := table[i*packedRecordIndexBytes : (i+1)*packedRecordIndexBytes]
		copy(row[:32], hash)
		binary.BigEndian.PutUint64(row[32:40], uint64(data.Len()))
		binary.BigEndian.PutUint32(row[40:44], uint32(len(record)))
		sum := sha256.Sum256(record)
		copy(row[44:76], sum[:])
		_, _ = data.Write(record)
	}
	sum := sha256.Sum256(table)
	header := packedIndexHeader{Version: 1, Epoch: index.Epoch, Revision: index.Revision, Inventory: index.Inventory, Count: len(keys), Bytes: data.Len(), IndexChecksum: hex.EncodeToString(sum[:])}
	header.Checksum = header.checksum()
	metadata, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	result := make([]byte, 12+len(metadata)+len(table)+data.Len())
	copy(result, packedIndexMagic)
	binary.BigEndian.PutUint32(result[8:12], uint32(len(metadata)))
	offset := 12
	copy(result[offset:], metadata)
	offset += len(metadata)
	copy(result[offset:], table)
	offset += len(table)
	copy(result[offset:], data.Bytes())
	return result, nil
}

// Physical per-key files dominate packed authority, including damage. The same
// logical snapshot is compared before staging and again under the request lock.
func (s *Store) readIndexSnapshot(path string) (fileSnapshot, error) {
	before, err := readSnapshot(path)
	if err != nil || before.found {
		return before, err
	}
	if filepath.Dir(path) != filepath.Join(s.home, "sessions-v1") {
		return before, nil
	}
	name := filepath.Base(path)
	if len(name) != 69 || filepath.Ext(name) != ".json" {
		return before, ErrSessionIndexRecoveryRequired
	}
	var marker sessionIndexMarker
	err = local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker)
	if errors.Is(err, os.ErrNotExist) || (err == nil && marker.Version == 1) {
		return before, nil
	}
	if err != nil || marker.Version != 2 {
		return before, ErrSessionIndexRecoveryRequired
	}
	entry, found, err := s.readPackedEntry(name[:64], marker)
	if err != nil {
		if !marker.Complete {
			if _, statErr := os.Stat(packedIndexPath(s.home, name[:2])); errors.Is(statErr, os.ErrNotExist) {
				return before, nil
			}
		}
		return before, err
	}
	if !found {
		return before, nil
	}
	key := agentmeta.SessionKey{Agent: entry.Agent, NativeID: entry.NativeID}
	entry, found, err = s.packedSessionIndexEntry(key)
	if err != nil || !found {
		return fileSnapshot{}, err
	}
	data, err := json.Marshal(entry)
	return fileSnapshot{data: data, found: true}, err
}

func (s *Store) preparePackedSessionIndex(ctx context.Context, revision, inventory string) (sessionIndexMarker, error) {
	var marker sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil {
		return marker, err
	}
	if marker.Version == 2 && marker.PackedRevision == revision && marker.PackedInventory == inventory {
		return marker, nil
	}
	epoch, err := local.ID()
	if err != nil {
		return marker, err
	}
	err = s.writeUnderLock(lockedWrite{
		lock: func() (func(), error) { return s.namedLockWait("hooks.lock", time.Second) }, path: filepath.Join(s.home, sessionIndexMarkerFile), check: ctx.Err,
		change: func(current fileSnapshot) (any, bool, error) {
			if json.Unmarshal(current.data, &marker) != nil || !recoveryMarkerVersion(marker.Version) || marker.Complete {
				return nil, false, ErrSessionIndexRecoveryRequired
			}
			marker.Version = 2
			marker.PackedEpoch = epoch
			marker.PackedRevision = revision
			marker.PackedInventory = inventory
			return marker, true, nil
		},
	})
	return marker, err
}

func (s *Store) recoverPackedShard(ctx context.Context, shard string, marker sessionIndexMarker, owners map[agentmeta.SessionKey][]string, candidates []SubagentCandidate) error {
	entries := make(map[string]qualifiedSessionIndexEntry, len(owners)+len(candidates))
	ownerKeys, _ := inventoryFingerprint(owners)
	for _, key := range ownerKeys {
		ids := owners[key]
		if err := ctx.Err(); err != nil {
			return err
		}
		hash := packedIndexHash(key)
		entry := indexEntry(key, ids[0])
		if len(ids) > 1 {
			entry.ArchiveSessionID = ""
			entry.Conflict = true
		}
		// Existing overrides keep their authority; requests are repaired by the
		// original locked path, never hidden by an aggregate replacement.
		physical, err := readSnapshot(qualifiedSessionIndexPath(s.home, key))
		if err != nil {
			return err
		}
		if physical.found {
			var prior qualifiedSessionIndexEntry
			if json.Unmarshal(physical.data, &prior) != nil || prior.validate(key) != nil || prior.Reservation != "" || prior.ArchiveSessionID != ids[0] || len(ids) > 1 {
				if err := s.recoverRegistrationOwners(key, ids); err != nil {
					return err
				}
			}
		}
		entries[hash] = entry
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
		if err != nil || !safeFileComponent(candidate.ArchiveSessionID) {
			return ErrSessionIndexRecoveryRequired
		}
		hash := packedIndexHash(key)
		if existing, found := entries[hash]; found {
			if existing.Conflict || existing.ArchiveSessionID != candidate.ArchiveSessionID {
				return ErrSessionIdentityConflict
			}
			continue
		}
		parentKey, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.ParentNativeSessionID)
		if err != nil {
			return err
		}
		valid, err := s.matchingRegistration(parentKey, candidate.ParentArchiveSessionID)
		if err != nil {
			return err
		}
		if !valid {
			continue
		}
		physical, err := readSnapshot(qualifiedSessionIndexPath(s.home, key))
		if err != nil {
			return err
		}
		if physical.found {
			var prior qualifiedSessionIndexEntry
			if json.Unmarshal(physical.data, &prior) == nil && prior.validate(key) == nil && !prior.Absent {
				if prior.ArchiveSessionID != candidate.ArchiveSessionID {
					return ErrSessionIdentityConflict
				}
			} else if err := s.recoverCandidateIndex(ctx, candidate); err != nil {
				return err
			}
		}
		entry := indexEntry(key, candidate.ArchiveSessionID)
		entry.Reservation = marker.PackedEpoch
		entries[hash] = entry
	}
	index := packedSessionIndex{Version: 1, Epoch: marker.PackedEpoch, Revision: marker.PackedRevision, Inventory: marker.PackedInventory, Entries: entries}
	data, err := encodePackedIndex(index)
	if err != nil {
		return err
	}
	if len(entries) > packedSessionIndexMaxEntries || len(data) > packedSessionIndexMaxBytes {
		// Large/skewed existing identities remain supported by the ordinary durable
		// namespace. Bounds select storage, never reject otherwise valid input.

		// The marker is incomplete. Publish an empty current-epoch shard first so
		// ordinary locked repairs see logical absence, not an obsolete aggregate.
		// Failure leaves the certificate incomplete and the cursor on this shard.
		index.Entries = map[string]qualifiedSessionIndexEntry{}
		empty, err := encodePackedIndex(index)
		if err != nil {
			return err
		}
		if err := local.WriteBytes(packedIndexPath(s.home, shard), empty); err != nil {
			return err
		}
		keys, _ := inventoryFingerprint(owners)
		for _, key := range keys {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.recoverRegistrationOwners(key, owners[key]); err != nil {
				return err
			}
		}
		for _, candidate := range candidates {
			if err := s.recoverCandidateIndex(ctx, candidate); err != nil {
				return err
			}
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return local.WriteBytes(packedIndexPath(s.home, shard), data)
}

func packedRecoverySources(inventory map[agentmeta.SessionKey][]string, candidates []SubagentCandidate) ([packedSessionIndexShards]map[agentmeta.SessionKey][]string, [packedSessionIndexShards][]SubagentCandidate, error) {
	var owners [packedSessionIndexShards]map[agentmeta.SessionKey][]string
	var children [packedSessionIndexShards][]SubagentCandidate
	for key, ids := range inventory {
		hash := packedIndexHash(key)
		bytes, _ := hex.DecodeString(hash[:2])
		i := int(bytes[0])
		if owners[i] == nil {
			owners[i] = make(map[agentmeta.SessionKey][]string)
		}
		owners[i][key] = ids
	}
	for _, candidate := range candidates {
		key, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
		if err != nil {
			return owners, children, err
		}
		hash := packedIndexHash(key)
		bytes, _ := hex.DecodeString(hash[:2])
		i := int(bytes[0])
		children[i] = append(children[i], candidate)
	}
	return owners, children, nil
}

func packedShardName(index int) string { return fmt.Sprintf("%02x", index) }

func (s *Store) validatePackedPrefix(marker sessionIndexMarker, count int) (int, error) {
	for i := range count {
		if _, err := s.readPackedSessionIndex(packedShardName(i), marker); err != nil {
			return i, err
		}
	}
	return count, nil
}
