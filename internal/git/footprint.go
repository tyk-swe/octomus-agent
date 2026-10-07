package git

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
)

func ReadMaintenanceFootprint(ctx context.Context, cfg config.Config, workTree, base, revision string) (model.MaintenanceFootprint, error) {
	result := model.MaintenanceFootprint{
		ComparisonBase: base, Revision: revision, Paths: []string{}, ManualReasons: []string{},
	}
	if !footprintRevision(base) || !footprintRevision(revision) {
		return result, errors.New("Footprint requires exact hexadecimal comparison and output revisions")
	}
	scratch, err := os.MkdirTemp("", "octomus-footprint-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(scratch)
	empty := filepath.Join(scratch, "tree")
	if err := os.Mkdir(empty, 0o700); err != nil {
		return result, err
	}
	env := append(slices.Clone(isolatedConfig),
		"GIT_INDEX_FILE="+filepath.Join(scratch, "index"), "GIT_ATTR_NOSYSTEM=1")
	read := func(format string) (string, error) {
		args, err := metadataArgs(workTree, empty, []string{
			"-c", "core.attributesFile=" + os.DevNull, "diff", format, "-z",
			"--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
			"--ignore-submodules=none", "--submodule=short", base, revision, "--",
		})
		if err != nil {
			return "", err
		}
		return process.RunMachine(ctx, "git", args, empty, cfg.CommandTimeoutSeconds, env)
	}
	numstat, err := read("--numstat")
	if err != nil {
		return result, err
	}
	raw, err := read("--raw")
	if err != nil {
		return result, err
	}
	return parseMaintenanceFootprint(numstat, raw, base, revision, cfg.AutoMergeExcludedPaths)
}

func footprintRevision(revision string) bool {
	if len(revision) != 40 && len(revision) != 64 {
		return false
	}
	for _, c := range revision {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func parseMaintenanceFootprint(numstat, raw, base, revision string, excluded []string) (model.MaintenanceFootprint, error) {
	result := model.MaintenanceFootprint{
		ComparisonBase: base, Revision: revision, Paths: []string{}, ManualReasons: []string{},
	}
	entries, err := footprintRecords(numstat)
	if err != nil {
		return result, err
	}
	modes, err := footprintModes(raw)
	if err != nil {
		return result, err
	}
	seen := map[string]bool{}
	var lines, files uint64
	countable := true
	for _, entry := range entries {
		fields := strings.SplitN(entry, "\t", 3)
		if len(fields) != 3 || fields[2] == "" || seen[fields[2]] {
			return result, errors.New("Footprint numstat contains an invalid or duplicate path")
		}
		name := fields[2]
		mode, ok := modes[name]
		if !ok {
			return result, errors.New("Footprint numstat and raw paths disagree")
		}
		seen[name] = true
		result.Paths = append(result.Paths, name)
		if files == math.MaxUint64 {
			return result, errors.New("Footprint file count overflows")
		}
		files++
		if fields[0] == "-" || fields[1] == "-" {
			if fields[0] != "-" || fields[1] != "-" {
				return result, errors.New("Footprint contains inconsistent binary line counts")
			}
			countable = false
			result.ManualReasons = append(result.ManualReasons, footprintReason("Unquantifiable binary change", name))
		} else {
			for _, text := range fields[:2] {
				if text == "" || strings.Trim(text, "0123456789") != "" {
					return result, errors.New("Footprint line count is not an unsigned decimal integer")
				}
				n, err := strconv.ParseUint(text, 10, 64)
				if err != nil || n > math.MaxUint64-lines {
					return result, errors.New("Footprint line count overflows")
				}
				lines += n
			}
		}
		if !regularFootprintMode(mode[0]) || !regularFootprintMode(mode[1]) {
			result.ManualReasons = append(result.ManualReasons, footprintReason("Special file or submodule change", name))
		} else if mode[0] != mode[1] && (mode[0] == "100755" || mode[1] == "100755") {
			result.ManualReasons = append(result.ManualReasons, footprintReason("Executable mode change", name))
		}
		if config.ManualMergePath(name, excluded) {
			result.ManualReasons = append(result.ManualReasons, footprintReason("Sensitive or excluded path", name))
		}
	}
	if len(seen) != len(modes) {
		return result, errors.New("Footprint raw and numstat paths disagree")
	}
	result.ChangedFiles = &files
	if countable {
		result.ChangedLines = &lines
	}
	result.Complete = true
	return result, nil
}

func footprintRecords(data string) ([]string, error) {
	if data == "" {
		return []string{}, nil
	}
	if !strings.HasSuffix(data, "\x00") {
		return nil, errors.New("Footprint machine output is incomplete")
	}
	return strings.Split(strings.TrimSuffix(data, "\x00"), "\x00"), nil
}

func footprintModes(raw string) (map[string][2]string, error) {
	records, err := footprintRecords(raw)
	if err != nil {
		return nil, err
	}
	if len(records)%2 != 0 {
		return nil, errors.New("Footprint raw metadata is incomplete")
	}
	result := map[string][2]string{}
	for i := 0; i < len(records); i += 2 {
		header, name := records[i], records[i+1]
		if !strings.HasPrefix(header, ":") || name == "" {
			return nil, errors.New("Footprint raw metadata is malformed")
		}
		fields := strings.Fields(strings.TrimPrefix(header, ":"))
		if len(fields) != 5 || !footprintMode(fields[0]) || !footprintMode(fields[1]) ||
			!footprintObjectFragment(fields[2]) || !footprintObjectFragment(fields[3]) ||
			len(fields[4]) != 1 || !strings.Contains("ADMT", fields[4]) {
			return nil, errors.New("Footprint raw metadata has an unexpected mode or status")
		}
		if fields[4] == "A" && (fields[0] != "000000" || fields[1] == "000000") ||
			fields[4] == "D" && (fields[0] == "000000" || fields[1] != "000000") ||
			strings.Contains("MT", fields[4]) && (fields[0] == "000000" || fields[1] == "000000") {
			return nil, errors.New("Footprint raw modes disagree with the change status")
		}
		if _, exists := result[name]; exists {
			return nil, errors.New("Footprint raw metadata repeats a path")
		}
		result[name] = [2]string{fields[0], fields[1]}
	}
	return result, nil
}

func footprintMode(mode string) bool {
	return len(mode) == 6 && strings.Trim(mode, "01234567") == ""
}

func footprintObjectFragment(value string) bool {
	return value != "" && len(value) <= 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func regularFootprintMode(mode string) bool {
	return mode == "000000" || mode == "100644" || mode == "100755"
}

func footprintReason(reason, name string) string {
	return fmt.Sprintf("%s at %s", reason, strconv.Quote(redact.Text(name)))
}

func maintenanceRemoteFootprintWithin(additions, deletions, files uint64, saved, live config.Config) bool {
	maxLines := min(saved.AutoMergeMaxLines, live.AutoMergeMaxLines)
	maxFiles := min(saved.AutoMergeMaxFiles, live.AutoMergeMaxFiles)
	return maxLines > 0 && maxFiles > 0 && files > 0 && files <= maxFiles &&
		additions <= math.MaxUint64-deletions && additions+deletions <= maxLines
}
