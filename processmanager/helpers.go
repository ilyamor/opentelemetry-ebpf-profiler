// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package processmanager // import "go.opentelemetry.io/ebpf-profiler/processmanager"

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"

	lru "github.com/elastic/go-freelru"
	log "github.com/sirupsen/logrus"

	"go.opentelemetry.io/ebpf-profiler/host"
	"go.opentelemetry.io/ebpf-profiler/libpf"
	"go.opentelemetry.io/ebpf-profiler/reporter"
)

//nolint:lll
var (
	cgroupv2ContainerIDPattern = regexp.MustCompile(`0:.*?:.*?([0-9a-fA-F]{64})(?:\.scope)?(?:/[a-z]+)?$`)
)

type lruFileIDMapper struct {
	cache *lru.SyncedLRU[host.FileID, libpf.FrameMappingFile]
}

// identityHash maps the host.FileID to a 32bit value.
// No need to explicitly hash the FileID, since it's already a hash value.
func identityHash(key host.FileID) uint32 {
	return uint32(key)
}

func newFileIDMapper(size int) (*lruFileIDMapper, error) {
	cache, err := lru.NewSynced[host.FileID, libpf.FrameMappingFile](uint32(size), identityHash)
	if err != nil {
		return nil, err
	}
	return &lruFileIDMapper{cache}, nil
}

func (fm *lruFileIDMapper) Get(key host.FileID) (libpf.FrameMappingFile, bool) {
	if mappingFile, ok := fm.cache.Get(key); ok {
		return mappingFile, true
	}

	log.Warnf("Failed to lookup file ID %#x", key)
	return libpf.FrameMappingFile{}, false
}

func (fm *lruFileIDMapper) Set(key host.FileID, val libpf.FrameMappingFile) {
	fm.cache.Add(key, val)
	log.Debugf("Stored file ID mapping %#x -> %#x", key, val.Value())
}

var _ FileIDMapper = (*lruFileIDMapper)(nil)

// MapFileIDMapper implements the FileIDMApper using a map (for testing)
type MapFileIDMapper struct {
	fileMap map[host.FileID]libpf.FrameMappingFile
}

func NewMapFileIDMapper() *MapFileIDMapper {
	return &MapFileIDMapper{
		fileMap: make(map[host.FileID]libpf.FrameMappingFile),
	}
}

func (fm *MapFileIDMapper) Get(key host.FileID) (libpf.FrameMappingFile, bool) {
	if value, ok := fm.fileMap[key]; ok {
		return value, true
	}
	return libpf.FrameMappingFile{}, true
}

func (fm *MapFileIDMapper) Set(key host.FileID, value libpf.FrameMappingFile) {
	fm.fileMap[key] = value
}

var _ FileIDMapper = (*MapFileIDMapper)(nil)

// FileIDMapper is responsible for mapping between 64-bit file IDs to the frame mapping metadata.
type FileIDMapper interface {
	// Retrieve the metadata for given 64-bit file ID.
	Get(fileID host.FileID) (libpf.FrameMappingFile, bool)
	// Associate the metadata for given 64-bit file ID.
	Set(fileID host.FileID, metadata libpf.FrameMappingFile)
}

// executableReporterStub is a stub to implement reporter.ExecutableReporter which is used
// as the reporter by default. This can be overridden on at processmanager creation time.
type executableReporterStub struct {
}

// ReportExecutable satisfies the reporter.ExecutableReporter interface.
func (er executableReporterStub) ReportExecutable(args *reporter.ExecutableMetadata) {

	go func() {
		log.Infof(
			"executableReporterStub.ReportExecutable called: (buildID: %s. fileId %s, name: %s) mapping path: %s, debuglink: %s",
			args.MappingFile.Value().GnuBuildID,
			args.MappingFile.Value().FileID,
			args.MappingFile.Value().FileName,
			args.Mapping.Path,
			args.DebuglinkFileName,
		)

		// Upload symbols to Coralogix
		if args.Mapping.Path != libpf.NullString {
			file, err := args.Process.ExtractAsFile(args.Mapping.Path.String())
			if err != nil {
				log.Errorf("Failed to extract executable file %s: %v", args.Mapping.Path, err)
			}
			if err := uploadSymbols(file); err != nil {
				log.Errorf("Failed to upload symbols for mapping %s: %v", file, err)
			}
		}
	}()
}

var _ reporter.ExecutableReporter = executableReporterStub{}

// parseContainerID parses cgroup v2 container IDs
func parseContainerID(cgroupFile io.Reader) string {
	scanner := bufio.NewScanner(cgroupFile)
	buf := make([]byte, 512)
	// Providing a predefined buffer overrides the internal buffer that Scanner uses (4096 bytes).
	// We can do that and also set a maximum allocation size on the following call.
	// With a maximum of 4096 characters path in the kernel, 8192 should be fine here. We don't
	// expect lines in /proc/<PID>/cgroup to be longer than that.
	scanner.Buffer(buf, 8192)
	var pathParts []string
	for scanner.Scan() {
		b := scanner.Bytes()
		if bytes.Equal(b, []byte("0::/")) {
			continue // Skip a common case
		}
		line := string(b)
		pathParts = cgroupv2ContainerIDPattern.FindStringSubmatch(line)
		if pathParts == nil {
			log.Debugf("Could not extract cgroupv2 path from line: %s", line)
			continue
		}
		return pathParts[1]
	}

	// No containerID could be extracted
	return ""
}

// extractContainerID returns the containerID for pid if cgroup v2 is used.
func extractContainerID(pid libpf.PID) (string, error) {
	cgroupFile, err := os.Open(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}

	return parseContainerID(cgroupFile), nil
}

// uploadSymbols executes the symbol-upload-tool command to upload symbols to Coralogix
func uploadSymbols(mappingPath string) error {
	// Extract environment variables
	token := os.Getenv("token")
	if token == "" {
		return fmt.Errorf("token environment variable not set")
	}

	symbolURL := os.Getenv("SYMBOL_URL")
	if symbolURL == "" {
		return fmt.Errorf("SYMBOL_URL environment variable not set")
	}

	// Build the command
	cmd := exec.Command("symbol-upload-tool", "upload",
		"--coralogix-endpoint", symbolURL,
		"--auth", token,
		mappingPath)

	// Execute the command and capture output
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Errorf("Failed to upload symbols for %s: %v, output: %s", mappingPath, err, string(output))
		return fmt.Errorf("symbol upload failed: %w", err)
	}

	log.Infof("Successfully uploaded symbols for %s: %s", mappingPath, string(output))
	return nil
}
