package weedcli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var errorKeywords = []string{
	"error:",
	"failed to",
	"cannot find",
	"not found",
	"unknown command",
	"connection refused",
	"rpc error:",
	"failed to acquire master lock",
	"panic:",
}

// DetectInBandError inspects process exit code and output lines for error signatures.
func DetectInBandError(stdout, stderr string, exitCode int) error {
	if exitCode != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = strings.TrimSpace(stdout)
		}
		if msg == "" {
			msg = fmt.Sprintf("process exited with status %d", exitCode)
		}
		return fmt.Errorf("%w: exit code %d: %s", ErrCommandExecutionFailed, exitCode, msg)
	}

	combined := stdout + "\n" + stderr
	scanner := bufio.NewScanner(strings.NewReader(combined))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		for _, kw := range errorKeywords {
			if strings.Contains(lower, kw) {
				// Filter false positives in normal informational/summary logs
				if strings.Contains(lower, "0 error") || strings.Contains(lower, "no error") || strings.Contains(lower, "0 errors") {
					continue
				}
				return fmt.Errorf("%w: %s", ErrCommandExecutionFailed, line)
			}
		}
	}
	return nil
}

// VolumeTopology models the parsed output of volume.list.
type VolumeTopology struct {
	VolumeSizeLimitMB int64            `json:"volumeSizeLimitMb"`
	DataCenters       []DataCenterInfo `json:"dataCenters"`
	TotalVolumes      int              `json:"totalVolumes"`
	ActiveVolumes     int              `json:"activeVolumes"`
	FreeVolumes       int              `json:"freeVolumes"`
}

type DataCenterInfo struct {
	Name  string     `json:"name"`
	Racks []RackInfo `json:"racks"`
}

type RackInfo struct {
	Name      string         `json:"name"`
	DataNodes []DataNodeInfo `json:"dataNodes"`
}

type DataNodeInfo struct {
	Address string       `json:"address"`
	Volumes []VolumeInfo `json:"volumes"`
}

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

// S3BucketInfo models parsed bucket details.
type S3BucketInfo struct {
	Name         string  `json:"name"`
	Size         int64   `json:"size"`
	Logical      int64   `json:"logical"`
	ChunkCount   int64   `json:"chunkCount"`
	Quota        int64   `json:"quota"`
	UsagePercent float64 `json:"usagePercent"`
}

// S3User models an IAM user parsed from s3.configure.
type S3User struct {
	Name        string         `json:"name"`
	Credentials []S3Credential `json:"credentials"`
	Actions     []string       `json:"actions"`
}

type S3Credential struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

type S3Configuration struct {
	Identities []S3Identity `json:"identities"`
}

type S3Identity struct {
	Name        string         `json:"name"`
	Credentials []S3Credential `json:"credentials"`
	Actions     []string       `json:"actions"`
}

// FilerEntryMeta models metadata returned by fs.meta.cat.
type FilerEntryMeta struct {
	Name        string                 `json:"name"`
	IsDirectory bool                   `json:"isDirectory"`
	Chunks      []FilerChunkMeta       `json:"chunks"`
	Attributes  map[string]interface{} `json:"attributes"`
}

type FilerChunkMeta struct {
	FileID       string `json:"fileId"`
	Offset       int64  `json:"offset"`
	Size         uint64 `json:"size"`
	ModifiedTsNs int64  `json:"modifiedTsNs"`
	ETag         string `json:"eTag"`
}

var (
	reLimit    = regexp.MustCompile(`volumeSizeLimit:\s*(\d+)`)
	reDC       = regexp.MustCompile(`^\s*DC:\s*([^\s]+)`)
	reRack     = regexp.MustCompile(`^\s*Rack:\s*([^\s]+)`)
	reDataNode = regexp.MustCompile(`^\s*DataNode:\s*([^\s(]+)`)
	reFree     = regexp.MustCompile(`Free:\s*(\d+)`)
	reVolCount = regexp.MustCompile(`Volumes:\s*(\d+)/(\d+)`)
)

// ParseVolumeList parses the hierarchical text output of weed shell volume.list.
func ParseVolumeList(stdout string) (*VolumeTopology, error) {
	topo := &VolumeTopology{
		DataCenters: make([]DataCenterInfo, 0),
	}

	scanner := bufio.NewScanner(strings.NewReader(stdout))
	var currentDC *DataCenterInfo
	var currentRack *RackInfo
	var currentNode *DataNodeInfo

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if m := reLimit.FindStringSubmatch(line); len(m) > 1 {
			if limit, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				topo.VolumeSizeLimitMB = limit
			}
		}

		if m := reDC.FindStringSubmatch(line); len(m) > 1 {
			dcName := m[1]
			topo.DataCenters = append(topo.DataCenters, DataCenterInfo{
				Name:  dcName,
				Racks: make([]RackInfo, 0),
			})
			currentDC = &topo.DataCenters[len(topo.DataCenters)-1]
			currentRack = nil
			currentNode = nil
			continue
		}

		if m := reRack.FindStringSubmatch(line); len(m) > 1 && currentDC != nil {
			rackName := m[1]
			currentDC.Racks = append(currentDC.Racks, RackInfo{
				Name:      rackName,
				DataNodes: make([]DataNodeInfo, 0),
			})
			currentRack = &currentDC.Racks[len(currentDC.Racks)-1]
			currentNode = nil
			continue
		}

		if m := reDataNode.FindStringSubmatch(line); len(m) > 1 && currentRack != nil {
			nodeAddr := m[1]
			if fm := reFree.FindStringSubmatch(line); len(fm) > 1 {
				if free, err := strconv.Atoi(fm[1]); err == nil {
					topo.FreeVolumes += free
				}
			}
			currentRack.DataNodes = append(currentRack.DataNodes, DataNodeInfo{
				Address: nodeAddr,
				Volumes: make([]VolumeInfo, 0),
			})
			currentNode = &currentRack.DataNodes[len(currentRack.DataNodes)-1]
			continue
		}

		if strings.Contains(line, "volume id:") && currentNode != nil {
			v := parseVolumeLine(line)
			currentNode.Volumes = append(currentNode.Volumes, v)
			topo.TotalVolumes++
			if !v.ReadOnly {
				topo.ActiveVolumes++
			}
			continue
		}
	}

	return topo, nil
}

func parseVolumeLine(line string) VolumeInfo {
	v := VolumeInfo{
		ReplicaPlacement: "000",
	}

	parts := strings.Fields(line)
	for i := 0; i < len(parts); i++ {
		key := parts[i]
		if i+1 >= len(parts) {
			break
		}
		val := parts[i+1]

		switch key {
		case "id:":
			if id, err := strconv.ParseUint(val, 10, 32); err == nil {
				v.ID = uint32(id)
			}
		case "size:":
			if size, err := strconv.ParseUint(val, 10, 64); err == nil {
				v.Size = size
			}
		case "collection:":
			v.Collection = strings.Trim(val, `"'`)
		case "version:":
			if ver, err := strconv.Atoi(val); err == nil {
				v.Version = ver
			}
		case "read":
			if i+2 < len(parts) && parts[i+1] == "only:" {
				v.ReadOnly = parts[i+2] == "true"
			}
		}

		// Handle multi-word keys
		if i+2 < len(parts) {
			twoWords := key + " " + parts[i+1]
			val2 := parts[i+2]
			switch twoWords {
			case "replica placement:":
				v.ReplicaPlacement = val2
			case "file count:":
				if fc, err := strconv.ParseUint(val2, 10, 64); err == nil {
					v.FileCount = fc
				}
			case "delete count:":
				if dc, err := strconv.ParseUint(val2, 10, 64); err == nil {
					v.DeleteCount = dc
				}
			case "deleted byte":
				if i+3 < len(parts) && parts[i+2] == "count:" {
					if dbc, err := strconv.ParseUint(parts[i+3], 10, 64); err == nil {
						v.DeletedByteCount = dbc
					}
				}
			}
		}
	}

	if v.Size > 0 && v.DeletedByteCount > 0 {
		v.GarbageRatio = float64(v.DeletedByteCount) / float64(v.Size)
	}

	return v
}

// ParseS3BucketList parses lines or formatted tables from s3.bucket.list into bucket names and S3BucketInfo.
func ParseS3BucketList(stdout string) ([]string, []S3BucketInfo, error) {
	var names []string
	var buckets []S3BucketInfo

	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ">") {
			continue
		}
		// Ignore headers and separators
		lower := strings.ToLower(line)
		if lower == "name" || lower == "bucket" || lower == "bucket name" ||
			strings.HasPrefix(lower, "name ") || strings.HasPrefix(lower, "name\t") ||
			strings.HasPrefix(lower, "bucket name") || strings.HasPrefix(lower, "---") ||
			strings.Contains(lower, "volume.list") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		bucketName := fields[0]
		// Skip prompt lines or exit statements
		if bucketName == "exit" || bucketName == "lock" || bucketName == "unlock" {
			continue
		}

		names = append(names, bucketName)

		info := S3BucketInfo{
			Name: bucketName,
		}
		if len(fields) >= 2 {
			if sz, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				info.Size = sz
			}
		}
		buckets = append(buckets, info)
	}

	return names, buckets, nil
}

// ParseS3Configure extracts the identities and configuration JSON from s3.configure stdout.
func ParseS3Configure(stdout string) ([]S3User, *S3Configuration, error) {
	// Locate start of JSON array or object
	startIdx := strings.Index(stdout, "{")
	if startIdx == -1 {
		startIdx = strings.Index(stdout, "[")
	}
	if startIdx == -1 {
		return []S3User{}, &S3Configuration{}, nil
	}

	jsonBlob := stdout[startIdx:]
	// Try parsing as S3Configuration struct
	var config S3Configuration
	if err := json.Unmarshal([]byte(jsonBlob), &config); err == nil && len(config.Identities) > 0 {
		users := make([]S3User, len(config.Identities))
		for i, id := range config.Identities {
			users[i] = S3User{
				Name:        id.Name,
				Credentials: id.Credentials,
				Actions:     id.Actions,
			}
		}
		return users, &config, nil
	}

	// Try parsing as slice of identities
	var idSlice []S3Identity
	if err := json.Unmarshal([]byte(jsonBlob), &idSlice); err == nil {
		users := make([]S3User, len(idSlice))
		for i, id := range idSlice {
			users[i] = S3User{
				Name:        id.Name,
				Credentials: id.Credentials,
				Actions:     id.Actions,
			}
		}
		return users, &S3Configuration{Identities: idSlice}, nil
	}

	return []S3User{}, &S3Configuration{}, nil
}

// ParseMetaCat parses the JSON output of fs.meta.cat into a FilerEntryMeta.
func ParseMetaCat(raw string) (*FilerEntryMeta, error) {
	startIdx := strings.Index(raw, "{")
	if startIdx == -1 {
		return nil, fmt.Errorf("no JSON object found in fs.meta.cat output")
	}
	endIdx := strings.LastIndex(raw, "}")
	if endIdx == -1 || endIdx <= startIdx {
		return nil, fmt.Errorf("no complete JSON object found in fs.meta.cat output")
	}

	jsonBlob := raw[startIdx : endIdx+1]
	var meta FilerEntryMeta
	if err := json.Unmarshal([]byte(jsonBlob), &meta); err != nil {
		// Fallback: parse raw generic map
		var rawMap map[string]interface{}
		if errMap := json.Unmarshal([]byte(jsonBlob), &rawMap); errMap == nil {
			meta.Attributes = rawMap
			if name, ok := rawMap["FullPath"].(string); ok {
				meta.Name = name
			}
			return &meta, nil
		}
		return nil, fmt.Errorf("decode fs.meta.cat json: %w", err)
	}
	return &meta, nil
}

// ParseVacuumSummary scans volume.vacuum output for compacted volume count.
func ParseVacuumSummary(stdout string) (int, uint64) {
	reCount := regexp.MustCompile(`(?i)(?:vacuumed|compacted)\s+(\d+)\s+volumes`)
	reBytes1 := regexp.MustCompile(`(?i)(?:reclaimed|deleted)\s+(\d+)\s+bytes`)
	reBytes2 := regexp.MustCompile(`(?i)(\d+)\s+bytes\s+(?:reclaimed|deleted)`)

	compacted := 0
	if m := reCount.FindStringSubmatch(stdout); len(m) > 1 {
		compacted, _ = strconv.Atoi(m[1])
	}
	var reclaimed uint64
	if m := reBytes1.FindStringSubmatch(stdout); len(m) > 1 {
		reclaimed, _ = strconv.ParseUint(m[1], 10, 64)
	} else if m := reBytes2.FindStringSubmatch(stdout); len(m) > 1 {
		reclaimed, _ = strconv.ParseUint(m[1], 10, 64)
	}
	return compacted, reclaimed
}

// ParseBalanceSummary scans volume.balance output for moved volume count.
func ParseBalanceSummary(stdout string) int {
	reMoved := regexp.MustCompile(`(?i)(?:moved|balanced)\s+(\d+)\s+volumes`)
	if m := reMoved.FindStringSubmatch(stdout); len(m) > 1 {
		moved, _ := strconv.Atoi(m[1])
		return moved
	}
	return 0
}

// ParseFixReplicationSummary scans volume.fix.replication output for fixed count.
func ParseFixReplicationSummary(stdout string) int {
	reFixed := regexp.MustCompile(`(?i)(?:fixed|replicated)\s+(\d+)\s+volumes`)
	if m := reFixed.FindStringSubmatch(stdout); len(m) > 1 {
		fixed, _ := strconv.Atoi(m[1])
		return fixed
	}
	return 0
}

// ParseGrowSummary scans volume.grow output for created volume count.
func ParseGrowSummary(stdout string) (int, []uint32) {
	reGrow := regexp.MustCompile(`(?i)(?:created|grew)\s+(\d+)\s+volumes?`)
	count := 0
	if m := reGrow.FindStringSubmatch(stdout); len(m) > 1 {
		count, _ = strconv.Atoi(m[1])
	}
	return count, nil
}
