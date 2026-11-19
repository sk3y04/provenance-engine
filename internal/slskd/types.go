// Package slskd integrates Soulseek into provenance through an external,
// unmodified slskd instance's HTTP API.
//
// The package owns four things and nothing else:
//
//   - a typed client for the slskd REST surface (X-API-Key only, never
//     Authorization: Bearer, which slskd rejects with 401 invalid_token),
//   - a connection-profile store with masked secrets,
//   - a search coordinator that serialises search creation (slskd allows one
//     concurrent search per instance),
//   - a durable download-job store plus the reconciler/ingestor that turn a
//     completed transfer into a provenance capture.
//
// Soulseek credentials, shares, upload quotas and the peer connection itself
// stay in slskd's configuration. This package never writes slskd configuration:
// no POST /api/v0/options/yaml, no remote_configuration requirement, no
// modification of the upstream binary.
package slskd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MinVersion is the hard runtime floor. Older releases lack transfer batches
// (POST /api/v0/transfers/downloads/batches), which the durable job store needs
// for restart-safe destination pinning.
var MinVersion = Version{Major: 0, Minor: 26, Patch: 0}

// Version is a parsed slskd version. slskd reports dotted quads such as
// "0.26.0.0 (0.26.0.0+e42a525d)"; only the first three components are compared.
type Version struct {
	Major int
	Minor int
	Patch int
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// AtLeast reports whether v is greater than or equal to min.
func (v Version) AtLeast(min Version) bool {
	if v.Major != min.Major {
		return v.Major > min.Major
	}
	if v.Minor != min.Minor {
		return v.Minor > min.Minor
	}
	return v.Patch >= min.Patch
}

// parseVersion accepts a dotted numeric prefix and ignores any suffix, so
// "0.26.0.0 (0.26.0.0+e42a525d)" and "1.0.0-beta.1" both parse.
func parseVersion(raw string) (Version, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return Version{}, errors.New("empty version")
	}
	// Cut at the first character that cannot be part of a numeric version.
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= '0' && c <= '9') || c == '.' {
			end++
			continue
		}
		break
	}
	core := strings.Trim(s[:end], ".")
	parts := strings.Split(core, ".")
	if len(parts) < 2 || parts[0] == "" {
		return Version{}, fmt.Errorf("unparseable version %q", raw)
	}
	nums := make([]int, 0, 3)
	for _, p := range parts {
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, fmt.Errorf("unparseable version %q: %w", raw, err)
		}
		nums = append(nums, n)
	}
	if len(nums) < 2 {
		return Version{}, fmt.Errorf("unparseable version %q", raw)
	}
	v := Version{Major: nums[0], Minor: nums[1]}
	if len(nums) > 2 {
		v.Patch = nums[2]
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// SearchRequest is the POST /api/v0/searches body.
//
// SearchTimeoutMS is in MILLISECONDS. slskd's own Swagger and SearchRequest.cs
// both say seconds; the running 0.26.0 binary ends a search 53 ms after start
// for a value of 5 (docs/findings-slskd-0.26-contract.md §2). Sending seconds
// would make every search a thousand times longer than intended.
type SearchRequest struct {
	ID                       string `json:"id,omitempty"`
	SearchText               string `json:"searchText"`
	SearchTimeoutMS          int    `json:"searchTimeout,omitempty"`
	FileLimit                int    `json:"fileLimit,omitempty"`
	ResponseLimit            int    `json:"responseLimit,omitempty"`
	FilterResponses          *bool  `json:"filterResponses,omitempty"`
	MinimumResponseFileCount int    `json:"minimumResponseFileCount,omitempty"`
	MaximumPeerQueueLength   int    `json:"maximumPeerQueueLength,omitempty"`
	MinimumPeerUploadSpeed   int    `json:"minimumPeerUploadSpeed,omitempty"`
}

// Search is the search document slskd returns from POST /api/v0/searches and
// GET /api/v0/searches/{id}.
type Search struct {
	ID              string           `json:"id"`
	SearchText      string           `json:"searchText"`
	Token           int              `json:"token"`
	State           string           `json:"state"`
	IsComplete      bool             `json:"isComplete"`
	ResponseCount   int              `json:"responseCount"`
	FileCount       int              `json:"fileCount"`
	LockedFileCount int              `json:"lockedFileCount"`
	StartedAt       time.Time        `json:"startedAt"`
	EndedAt         *time.Time       `json:"endedAt,omitempty"`
	Responses       []SearchResponse `json:"responses,omitempty"`
}

// SearchResponse is one peer's answer to a search.
type SearchResponse struct {
	Username          string `json:"username"`
	Token             int    `json:"token"`
	HasFreeUploadSlot bool   `json:"hasFreeUploadSlot"`
	UploadSpeed       int    `json:"uploadSpeed"`
	QueueLength       int    `json:"queueLength"`
	FileCount         int    `json:"fileCount"`
	Files             []File `json:"files"`
	LockedFileCount   int    `json:"lockedFileCount"`
	LockedFiles       []File `json:"lockedFiles"`
}

// File is a file offered by a peer. Extension is reported as "" by 0.26.x even
// for ordinary files, so callers must derive the format from Filename.
type File struct {
	Filename   string `json:"filename"`
	Size       int64  `json:"size"`
	Extension  string `json:"extension,omitempty"`
	Code       int    `json:"code,omitempty"`
	BitRate    *int   `json:"bitRate,omitempty"`
	BitDepth   *int   `json:"bitDepth,omitempty"`
	SampleRate *int   `json:"sampleRate,omitempty"`
	Length     *int   `json:"length,omitempty"`
	IsLocked   bool   `json:"isLocked,omitempty"`
}

// ---------------------------------------------------------------------------
// Transfers
// ---------------------------------------------------------------------------

// EnqueueBatchRequest is the POST /api/v0/transfers/downloads/batches body.
type EnqueueBatchRequest struct {
	ID       string               `json:"id,omitempty"`
	SearchID string               `json:"searchId,omitempty"`
	Username string               `json:"username"`
	Files    []EnqueueBatchItem   `json:"files"`
	Options  *EnqueueBatchOptions `json:"options,omitempty"`
}

// EnqueueBatchItem is one file in a batch.
type EnqueueBatchItem struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

// EnqueueBatchOptions pins the destination subdirectory and the caller's own
// identifier, which is what makes a restarted slskd recoverable.
type EnqueueBatchOptions struct {
	Destination string `json:"destination,omitempty"`
	ExternalID  string `json:"externalId,omitempty"`
}

// EnqueueBatchResponse is returned by the batch endpoint. Failures lists the
// files slskd refused; a 207 carries both a batch and failures.
type EnqueueBatchResponse struct {
	Batch    *Batch                `json:"batch,omitempty"`
	Failures []EnqueueBatchFailure `json:"failures,omitempty"`
}

// EnqueueBatchFailure is one refused file.
type EnqueueBatchFailure struct {
	Filename string `json:"filename,omitempty"`
	Message  string `json:"message,omitempty"`
}

// Batch groups transfers created by one enqueue call.
type Batch struct {
	ID        string     `json:"id"`
	SearchID  string     `json:"searchId,omitempty"`
	Username  string     `json:"username,omitempty"`
	Direction string     `json:"direction,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	Transfers []Transfer `json:"transfers,omitempty"`
}

// Transfer is one download or upload record.
type Transfer struct {
	BatchID         string     `json:"batchId,omitempty"`
	ID              string     `json:"id"`
	Username        string     `json:"username,omitempty"`
	Direction       string     `json:"direction,omitempty"`
	Filename        string     `json:"filename,omitempty"`
	Size            int64      `json:"size"`
	Transferred     int64      `json:"bytesTransferred"`
	State           string     `json:"state"`
	RequestedAt     time.Time  `json:"requestedAt"`
	EnqueuedAt      *time.Time `json:"enqueuedAt,omitempty"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	EndedAt         *time.Time `json:"endedAt,omitempty"`
	AverageSpeed    float64    `json:"averageSpeed"`
	PlaceInQueue    *int       `json:"placeInQueue,omitempty"`
	Exception       string     `json:"exception,omitempty"`
	Attempts        int        `json:"attempts"`
	NextAttemptAt   *time.Time `json:"nextAttemptAt,omitempty"`
	Removed         bool       `json:"removed"`
	PercentComplete float64    `json:"percentComplete"`
}

// QueuePosition is the per-transfer place-in-queue probe.
type QueuePosition struct {
	Position int    `json:"position"`
	Username string `json:"username,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// ValidateRemotePath refuses a remote path that cannot be materialised on a
// Linux filesystem or that tries to escape the download root. slskd retries a
// >255-byte path segment forever inside incomplete/<user>/<dir>/<file> (#1818),
// so the check happens before an enqueue, never after.
func ValidateRemotePath(remote string) error {
	if strings.TrimSpace(remote) == "" {
		return errors.New("remote path is empty")
	}
	for _, seg := range strings.FieldsFunc(remote, func(r rune) bool { return r == '\\' || r == '/' }) {
		if seg == "" {
			continue
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("remote path %q contains a traversal segment", remote)
		}
		if strings.ContainsRune(seg, 0) {
			return fmt.Errorf("remote path %q contains a NUL byte", remote)
		}
		// The limit is bytes, not runes: a multibyte name is legal exactly as
		// long as its UTF-8 encoding fits Linux NAME_MAX.
		if len(seg) > 255 {
			return fmt.Errorf("remote path segment %q is %d bytes, above the 255-byte filesystem limit", truncateForMessage(seg), len(seg))
		}
		if !utf8.ValidString(seg) {
			return fmt.Errorf("remote path segment %q is not valid UTF-8", truncateForMessage(seg))
		}
	}
	return nil
}

func truncateForMessage(s string) string {
	if len(s) <= 40 {
		return s
	}
	return s[:40] + "..."
}

// ---------------------------------------------------------------------------
// Health / server / application
// ---------------------------------------------------------------------------

// Application is GET /api/v0/application.
type Application struct {
	Version struct {
		Full              string `json:"full"`
		Current           string `json:"current"`
		Latest            string `json:"latest"`
		IsUpdateAvailable bool   `json:"isUpdateAvailable"`
		IsCanary          bool   `json:"isCanary"`
		IsDevelopment     bool   `json:"isDevelopment"`
	} `json:"version"`
	PendingReconnect bool        `json:"pendingReconnect"`
	PendingRestart   bool        `json:"pendingRestart"`
	Server           ServerState `json:"server"`
}

// ServerState is GET /api/v0/server. The Swagger advertises a richer
// ServerState schema; the running binary returns this flattened client state
// (docs/findings-slskd-0.26-contract.md §7).
type ServerState struct {
	State           string `json:"state"`
	Address         string `json:"address,omitempty"`
	Username        string `json:"username,omitempty"`
	IsConnected     bool   `json:"isConnected"`
	IsConnecting    bool   `json:"isConnecting"`
	IsLoggedIn      bool   `json:"isLoggedIn"`
	IsLoggingIn     bool   `json:"isLoggingIn"`
	IsTransitioning bool   `json:"isTransitioning"`
}

// Options is the subset of GET /api/v0/options this integration reads. slskd
// returns a far larger document; unknown fields are ignored.
type Options struct {
	RemoteConfiguration  bool `json:"remoteConfiguration"`
	RemoteFileManagement bool `json:"remoteFileManagement"`
	Feature              struct {
		Swagger bool `json:"swagger"`
	} `json:"feature"`
	Shares struct {
		Directories []string `json:"directories"`
		Filters     []string `json:"filters"`
		Cache       struct {
			StorageMode string `json:"storageMode"`
		} `json:"cache"`
	} `json:"shares"`
	Flags struct {
		NoShareScan bool `json:"noShareScan"`
		Volatile    bool `json:"volatile"`
	} `json:"flags"`
	Transfers struct {
		Download struct {
			Destination struct {
				Permissions struct {
					Mode string `json:"mode"`
				} `json:"permissions"`
			} `json:"destination"`
		} `json:"download"`
	} `json:"transfers"`
}

// Event is the response from POST /api/v0/events/{type}.
type Event struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Version   int       `json:"version"`
}

// ---------------------------------------------------------------------------
// Shares, files and uploads
// ---------------------------------------------------------------------------

// Share is one configured share as reported by GET /api/v0/shares. The endpoint
// returns a map keyed "local", not by alias.
type Share struct {
	ID          string `json:"id"`
	Alias       string `json:"alias"`
	IsExcluded  bool   `json:"isExcluded"`
	LocalPath   string `json:"localPath"`
	Raw         string `json:"raw"`
	RemotePath  string `json:"remotePath"`
	Directories int    `json:"directories"`
	Files       int    `json:"files"`
}

// Directory is one shared directory from GET /api/v0/shares/contents. Name uses
// Windows-style backslashes, e.g. "Provenance\Album".
type Directory struct {
	Name      string       `json:"name"`
	FileCount int          `json:"fileCount"`
	Files     []SharedFile `json:"files"`
}

// SharedFile is a file inside a shared directory. Filename is relative to the
// directory; Extension is reliable here (unlike search results).
type SharedFile struct {
	Filename       string            `json:"filename"`
	Size           int64             `json:"size"`
	Extension      string            `json:"extension,omitempty"`
	Code           int               `json:"code,omitempty"`
	AttributeCount int               `json:"attributeCount,omitempty"`
	Attributes     []json.RawMessage `json:"attributes,omitempty"`
}

// FilesystemDirectory is GET /api/v0/files/downloads/directories.
type FilesystemDirectory struct {
	Name        string                `json:"name"`
	FullName    string                `json:"fullName"`
	Attributes  string                `json:"attributes"`
	CreatedAt   time.Time             `json:"createdAt"`
	ModifiedAt  time.Time             `json:"modifiedAt"`
	Files       []FilesystemFile      `json:"files"`
	Directories []FilesystemDirectory `json:"directories"`
}

// FilesystemFile is one file on the slskd host.
type FilesystemFile struct {
	Name       string    `json:"name"`
	FullName   string    `json:"fullName"`
	Length     int64     `json:"length"`
	Attributes string    `json:"attributes"`
	CreatedAt  time.Time `json:"createdAt"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

// UserStatus is GET /api/v0/users/{username}/status.
type UserStatus struct {
	IsPrivileged bool   `json:"isPrivileged"`
	Presence     string `json:"presence"`
}

// UserInfo is GET /api/v0/users/{username}/info.
type UserInfo struct {
	Description       string `json:"description"`
	HasFreeUploadSlot bool   `json:"hasFreeUploadSlot"`
	HasPicture        bool   `json:"hasPicture"`
	QueueLength       int    `json:"queueLength"`
	UploadSlots       int    `json:"uploadSlots"`
}
