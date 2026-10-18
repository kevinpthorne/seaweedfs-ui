package weedhttp

import (
	"path"
	"strings"
	"time"
)

const (
	// ModeDirectory is the POSIX directory bit mask (1 << 31) matching os.ModeDir.
	ModeDirectory uint32 = 1 << 31
	// ModeSymlink is the POSIX symlink bit mask (1 << 28) matching os.ModeSymlink.
	ModeSymlink uint32 = 1 << 28
	// ModePermissionMask extracts standard rwxrwxrwx bits.
	ModePermissionMask uint32 = 0o777
)

// ClusterStatus represents the SeaweedFS Master leader election and Raft peers state.
type ClusterStatus struct {
	IsLeader bool     `json:"IsLeader"`
	Leader   string   `json:"Leader"`
	Peers    []string `json:"Peers"`
}

// DirStatusResponse models the raw output of Master GET /dir/status.
type DirStatusResponse struct {
	Topology DirTopology `json:"Topology"`
	Version  string      `json:"Version"`
}

// DirTopology models the data center hierarchy and volume slot limits.
type DirTopology struct {
	Max         int               `json:"Max"`
	Free        int               `json:"Free"`
	DataCenters []DirDataCenter   `json:"DataCenters"`
	Layouts     []DirVolumeLayout `json:"Layouts"`
}

// DirDataCenter models a DataCenter in the SeaweedFS topology.
type DirDataCenter struct {
	Id    string    `json:"Id"`
	Racks []DirRack `json:"Racks"`
}

// DirRack models a Rack in the SeaweedFS topology.
type DirRack struct {
	Id        string        `json:"Id"`
	DataNodes []DirDataNode `json:"DataNodes"`
}

// DirDataNode models a Volume server DataNode.
type DirDataNode struct {
	Url       string `json:"Url"`
	PublicUrl string `json:"PublicUrl,omitempty"`
	Volumes   int    `json:"Volumes"`
	Max       int    `json:"Max"`
	Free      int    `json:"Free"`
}

// DirVolumeLayout models volume collection allocation.
type DirVolumeLayout struct {
	Collection  string   `json:"collection"`
	Replication string   `json:"replication"`
	Writables   []uint32 `json:"writables"`
}

// VolumeStatusResponse models the output of Master GET /vol/status.
type VolumeStatusResponse struct {
	Version string           `json:"Version"`
	Volumes []VolumeDiskInfo `json:"Volumes"`
}

// VolumeDiskInfo models per-volume disk usage and statistics.
type VolumeDiskInfo struct {
	Id               uint32 `json:"Id"`
	Size             uint64 `json:"Size"`
	ReplicaPlacement int    `json:"ReplicaPlacement"`
	Ttl              string `json:"Ttl"`
	Collection       string `json:"Collection"`
	Version          int    `json:"Version"`
	FileCount        int64  `json:"FileCount"`
	DeleteCount      int64  `json:"DeleteCount"`
	DeletedByteCount uint64 `json:"DeletedByteCount"`
	ReadOnly         bool   `json:"ReadOnly"`
}

// VolumeInfo represents a normalized volume description used by UI tables.
type VolumeInfo struct {
	ID               uint32  `json:"id"`
	Size             uint64  `json:"size"`
	ReplicaPlacement string  `json:"replicaPlacement"`
	Collection       string  `json:"collection"`
	Version          int     `json:"version"`
	FileCount        uint64  `json:"fileCount"`
	DeleteCount      uint64  `json:"deleteCount"`
	DeletedByteCount uint64  `json:"deletedByteCount"`
	ReadOnly         bool    `json:"readOnly"`
	GarbageRatio     float64 `json:"garbageRatio"` // DeletedByteCount / Size if Size > 0
	ModifiedAtSecond int64   `json:"modifiedAtSecond,omitempty"`
}

// Topology represents the normalized cluster topology structure.
type Topology struct {
	MaxVolumeSlots  int               `json:"maxVolumeSlots"`
	FreeVolumeSlots int               `json:"freeVolumeSlots"`
	DataCenters     []DirDataCenter   `json:"dataCenters"`
	Layouts         []DirVolumeLayout `json:"layouts"`
	Version         string            `json:"version"`
}

// FilerDirResult represents a paginated directory listing result.
type FilerDirResult struct {
	Path                  string       `json:"Path"`
	Entries               []FilerEntry `json:"Entries"`
	Limit                 int          `json:"Limit"`
	LastFileName          string       `json:"LastFileName"`
	ShouldDisplayLoadMore bool         `json:"ShouldDisplayLoadMore"`
}

// FilerEntry represents a single file or directory inode in the Filer.
type FilerEntry struct {
	FullPath      string            `json:"FullPath"`
	Mtime         time.Time         `json:"Mtime"`
	Crtime        time.Time         `json:"Crtime"`
	Mode          uint32            `json:"Mode"`
	Uid           uint32            `json:"Uid"`
	Gid           uint32            `json:"Gid"`
	Mime          string            `json:"Mime"`
	Replication   string            `json:"Replication"`
	Collection    string            `json:"Collection"`
	TtlSec        int               `json:"TtlSec"`
	UserName      string            `json:"UserName"`
	GroupNames    []string          `json:"GroupNames"`
	SymlinkTarget string            `json:"SymlinkTarget"`
	Md5           string            `json:"Md5"`
	FileSize      uint64            `json:"FileSize"`
	Chunks        []FilerChunkEntry `json:"chunks"`
}

// FilerChunkEntry represents a storage chunk reference on a volume server.
type FilerChunkEntry struct {
	FileId string `json:"file_id"`
	Size   uint64 `json:"size"`
	Mtime  int64  `json:"mtime"`
	ETag   string `json:"e_tag"`
}

// IsDir returns true if the entry is a directory (Mode & (1 << 31) != 0).
func (e FilerEntry) IsDir() bool {
	return (e.Mode & ModeDirectory) != 0
}

// IsSymlink returns true if the entry is a symlink (Mode & (1 << 28) != 0).
func (e FilerEntry) IsSymlink() bool {
	return (e.Mode & ModeSymlink) != 0
}

// Name returns the base name of the entry.
func (e FilerEntry) Name() string {
	if e.FullPath == "" {
		return ""
	}
	clean := strings.TrimSuffix(e.FullPath, "/")
	return path.Base(clean)
}

// Size returns the effective byte size of the entry.
func (e FilerEntry) Size() uint64 {
	if e.FileSize > 0 {
		return e.FileSize
	}
	var total uint64
	for _, chunk := range e.Chunks {
		total += chunk.Size
	}
	return total
}

// Permissions returns the standard POSIX permission string (e.g. "drwxr-xr-x" or "-rw-r--r--").
func (e FilerEntry) Permissions() string {
	var buf [10]byte
	if e.IsDir() {
		buf[0] = 'd'
	} else if e.IsSymlink() {
		buf[0] = 'l'
	} else {
		buf[0] = '-'
	}

	perm := e.Mode & ModePermissionMask
	const rwx = "rwxrwxrwx"
	for i := 0; i < 9; i++ {
		if (perm & (1 << uint(8-i))) != 0 {
			buf[i+1] = rwx[i]
		} else {
			buf[i+1] = '-'
		}
	}
	return string(buf[:])
}

// S3EndpointStatus represents the health and reachability of the S3 gateway.
type S3EndpointStatus struct {
	Available  bool   `json:"available"`
	Endpoint   string `json:"endpoint"`
	StatusCode int    `json:"statusCode"`
	LatencyMs  int64  `json:"latencyMs"`
	Message    string `json:"message,omitempty"`
}

// VolumeServerStatus represents health data from a Volume server's GET /status.
type VolumeServerStatus struct {
	Version string           `json:"Version"`
	Volumes []VolumeDiskInfo `json:"Volumes"`
}
