package bittorrent

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"
)

// Identity is the canonical identity of one torrent.
//
// A v1 torrent has a 20-byte SHA-1 infohash, a v2 torrent (BEP 52) has a
// 32-byte SHA-256 one, and a hybrid carries both because it contains both the
// v1 and the v2 info structures. The full 32 bytes are the v2 identity: the
// library's 20-byte "short" form (Torrent.InfoHash) is a storage and wire
// convenience, never a durable identity here.
type Identity struct {
	V1    metainfo.Hash
	V2    infohash_v2.T
	HasV1 bool
	HasV2 bool
}

// ErrUnsupportedIdentity rejects a torrent with no identity this adapter can
// represent. A hybrid is supported; a v2-only torrent is supported; a
// dictionary with neither v1 piece hashes nor a v2 file tree is not.
var ErrUnsupportedIdentity = errors.New("unsupported torrent identity")

// IdentityFromMetaInfo derives both identities from exact metainfo bytes. The
// v1 hash covers the same info dictionary as the v2 hash, which is what makes
// them a matched pair for a hybrid.
func IdentityFromMetaInfo(mi *metainfo.MetaInfo) (Identity, error) {
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return Identity{}, fmt.Errorf("parse info: %w", err)
	}
	return IdentityFromInfo(&info, mi.InfoBytes)
}

// IdentityFromInfo derives both identities from a parsed info dictionary and the
// exact bytes it was decoded from.
func IdentityFromInfo(info *metainfo.Info, infoBytes []byte) (Identity, error) {
	if info == nil || len(infoBytes) == 0 {
		return Identity{}, fmt.Errorf("%w: no info dictionary", ErrUnsupportedIdentity)
	}
	var id Identity
	if info.HasV1() {
		if len(info.Pieces) == 0 {
			return Identity{}, fmt.Errorf("%w: v1 info dictionary has no piece hashes", ErrUnsupportedIdentity)
		}
		id.V1 = metainfo.HashBytes(infoBytes)
		id.HasV1 = true
	}
	if info.HasV2() {
		id.V2 = infohash_v2.HashBytes(infoBytes)
		id.HasV2 = true
	}
	if !id.HasV1 && !id.HasV2 {
		return Identity{}, fmt.Errorf("%w: metainfo has neither v1 piece hashes nor a v2 file tree", ErrUnsupportedIdentity)
	}
	return id, nil
}

// V1Hex is the full v1 infohash, or "" when the torrent has no v1 identity.
func (i Identity) V1Hex() string {
	if !i.HasV1 {
		return ""
	}
	return i.V1.HexString()
}

// V2Hex is the full 32-byte v2 infohash, or "" when there is no v2 identity.
func (i Identity) V2Hex() string {
	if !i.HasV2 {
		return ""
	}
	h := i.V2
	return h.HexString()
}

// JobID is the durable job identifier. v1 and hybrid torrents keep the original
// "bt-<40 hex>" form, so every existing job, capture key and status lookup keeps
// working. A v2-only torrent gets its own namespace, "bt2-<64 hex>", so it can
// never be mistaken for a truncated v1 hash or a v1 job.
func (i Identity) JobID() string {
	if i.HasV1 {
		return "bt-" + i.V1Hex()
	}
	return "bt2-" + i.V2Hex()
}

// StorageKey is the 20-byte infohash the pinned library uses for its piece
// completion database and storage namespace: the v1 hash when there is one,
// otherwise the library's short (truncated) form of the v2 hash. It is used
// only where the library requires it and is never persisted as an identity.
func (i Identity) StorageKey() metainfo.Hash {
	if i.HasV1 {
		return i.V1
	}
	if short := i.V2.ToShort(); short != nil {
		return *short
	}
	return metainfo.Hash{}
}

// MetainfoFile is the file name of the persisted metainfo for this identity.
// v1 and hybrid jobs keep "<v1 hex>.torrent"; a v2-only job uses the full v2
// hash, so the two namespaces cannot collide.
func (i Identity) MetainfoFile() string {
	if i.HasV1 {
		return i.V1Hex() + ".torrent"
	}
	return i.V2Hex() + ".torrent"
}

// Source is the sanitised source reference recorded on jobs and captures. Only a
// v1 identity has one: this adapter does not accept btmh magnets, so a v2-only
// job records no magnet at all rather than inventing one.
func (i Identity) Source() string {
	if i.HasV1 {
		return "magnet:?xt=urn:btih:" + i.V1Hex()
	}
	return ""
}

// Compare reports whether a lookup identity refers to this torrent: a hybrid
// matches a lookup by either of its hashes, and two identities of the same kind
// must agree. It is deliberately weaker than SameTorrent, which is what record
// validation uses.
func (i Identity) Compare(other Identity) bool {
	matched := false
	if i.HasV1 && other.HasV1 {
		if i.V1 != other.V1 {
			return false
		}
		matched = true
	}
	if i.HasV2 && other.HasV2 {
		if i.V2 != other.V2 {
			return false
		}
		matched = true
	}
	return matched
}

// SameTorrent reports whether two identities name the same torrent exactly
// (both hashes present and equal), or the same single-identity torrent.
func (i Identity) SameTorrent(other Identity) bool {
	if i.HasV1 != other.HasV1 || i.HasV2 != other.HasV2 {
		return false
	}
	if i.HasV1 && i.V1 != other.V1 {
		return false
	}
	if i.HasV2 && i.V2 != other.V2 {
		return false
	}
	return true
}

// String is a short, human-readable identity: one hash or both.
func (i Identity) String() string {
	switch {
	case i.HasV1 && i.HasV2:
		return "v1:" + i.V1Hex() + " v2:" + i.V2Hex()
	case i.HasV1:
		return "v1:" + i.V1Hex()
	default:
		return "v2:" + i.V2Hex()
	}
}

// parseIdentityString accepts the durable identifier forms this adapter writes:
// "bt-<40 hex>", "bt2-<64 hex>", or a bare 40/64 hex hash. It is the only way an
// external string becomes an identity: no display name, no truncation, no
// prefix guessing.
func parseIdentityString(raw string) (Identity, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.HasPrefix(s, "bt2-"):
		v2, err := parseV2Hex(strings.TrimPrefix(s, "bt2-"))
		if err != nil {
			return Identity{}, fmt.Errorf("%w: %q (want bt2-<64 hex>)", ErrInvalidJobID, raw)
		}
		return Identity{V2: v2, HasV2: true}, nil
	case strings.HasPrefix(s, "bt-"):
		v1, err := parseInfoHash(strings.TrimPrefix(s, "bt-"))
		if err != nil {
			return Identity{}, fmt.Errorf("%w: %q (want bt-<40 hex>)", ErrInvalidJobID, raw)
		}
		return Identity{V1: v1, HasV1: true}, nil
	}
	if len(s) == 64 {
		v2, err := parseV2Hex(s)
		if err != nil {
			return Identity{}, fmt.Errorf("%w: %q", ErrInvalidJobID, raw)
		}
		return Identity{V2: v2, HasV2: true}, nil
	}
	v1, err := parseInfoHash(s)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %q (want bt-<40 hex>, bt2-<64 hex>, or a bare hash)", ErrInvalidJobID, raw)
	}
	return Identity{V1: v1, HasV1: true}, nil
}

// parseV2Hex decodes a full 32-byte v2 infohash without truncating it.
func parseV2Hex(s string) (infohash_v2.T, error) {
	var h infohash_v2.T
	if len(s) != len(h)*2 {
		return h, fmt.Errorf("want %d hex characters", len(h)*2)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, err
	}
	copy(h[:], b)
	return h, nil
}

// identityFromJob derives the identity recorded on a job record. Exactly one
// combination is valid: a v1 hash, a v2 hash, or both (hybrid). The job id must
// agree with it.
func identityFromJob(job *jobRecord) (Identity, error) {
	var id Identity
	if v1 := strings.TrimSpace(job.InfoHash); v1 != "" {
		h, err := parseInfoHash(strings.ToLower(v1))
		if err != nil {
			return Identity{}, fmt.Errorf("job record %s has an invalid v1 infohash: %w", job.JobID, err)
		}
		id.V1, id.HasV1 = h, true
	}
	if v2 := strings.TrimSpace(job.InfoHashV2); v2 != "" {
		h, err := parseV2Hex(strings.ToLower(v2))
		if err != nil {
			return Identity{}, fmt.Errorf("job record %s has an invalid v2 infohash: %w", job.JobID, err)
		}
		id.V2, id.HasV2 = h, true
	}
	if !id.HasV1 && !id.HasV2 {
		return Identity{}, fmt.Errorf("%w: job record %s records no identity", ErrUnsupportedIdentity, job.JobID)
	}
	return id, nil
}
