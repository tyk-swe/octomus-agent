package git

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

func footprintRaw(oldMode, newMode, status, name string) string {
	return fmt.Sprintf(":%s %s 1234567 7654321 %s\x00%s\x00", oldMode, newMode, status, name)
}

func TestParseMaintenanceFootprintCounts(t *testing.T) {
	for _, test := range []struct {
		name, stat, raw string
		lines, files    uint64
	}{
		{
			"replacement-and-addition", "1\t1\told.txt\x001\t0\tnew.txt\x00",
			footprintRaw("100644", "100644", "M", "old.txt") +
				footprintRaw("000000", "100644", "A", "new.txt"), 3, 2,
		},
		{
			"uncollapsed-rename", "0\t1\tbefore.txt\x001\t0\tafter.txt\x00",
			footprintRaw("100644", "000000", "D", "before.txt") +
				footprintRaw("000000", "100644", "A", "after.txt"), 2, 2,
		},
		{
			"tab-and-newline-path", "1\t2\todd\tname\n.txt\x00",
			footprintRaw("100644", "100644", "M", "odd\tname\n.txt"), 3, 1,
		},
		{
			"largest-exact-unsigned-count", fmt.Sprintf("%d\t0\tfile.txt\x00", uint64(math.MaxUint64)),
			footprintRaw("100644", "100644", "M", "file.txt"), math.MaxUint64, 1,
		},
		{"empty", "", "", 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseMaintenanceFootprint(test.stat, test.raw, "base", "head", nil)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Complete || got.ChangedLines == nil || *got.ChangedLines != test.lines ||
				got.ChangedFiles == nil || *got.ChangedFiles != test.files ||
				uint64(len(got.Paths)) != test.files || len(got.ManualReasons) != 0 {
				t.Fatalf("footprint = %+v; want complete %d lines / %d paths", got, test.lines, test.files)
			}
			if got.ComparisonBase != "base" || got.Revision != "head" {
				t.Fatalf("footprint lost its revision binding: %+v", got)
			}
		})
	}
}

func TestParseMaintenanceFootprintManual(t *testing.T) {
	for _, test := range []struct {
		name, stat, raw string
		excluded        []string
		unknownLines    bool
	}{
		{"binary", "-\t-\tdata.bin\x00", footprintRaw("000000", "100644", "A", "data.bin"), nil, true},
		{"symlink", "1\t0\tlink\x00", footprintRaw("000000", "120000", "A", "link"), nil, false},
		{"submodule-removal", "0\t1\tvendor/module\x00", footprintRaw("160000", "000000", "D", "vendor/module"), nil, false},
		{"executable", "0\t0\tscript.sh\x00", footprintRaw("100644", "100755", "M", "script.sh"), nil, false},
		{"workflow", "1\t1\t.github/workflows/test.yml\x00", footprintRaw("100644", "100644", "M", ".github/workflows/test.yml"), nil, false},
		{"nested-migration", "1\t0\tinternal/store/migrations/fix.sql\x00", footprintRaw("000000", "100644", "A", "internal/store/migrations/fix.sql"), nil, false},
		{"extra-prefix", "1\t1\tinternal/access/rules.go\x00", footprintRaw("100644", "100644", "M", "internal/access/rules.go"), []string{"internal/access/"}, false},
		{"extra-name", "1\t1\tservice/special.policy\x00", footprintRaw("100644", "100644", "M", "service/special.policy"), []string{"special.policy"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseMaintenanceFootprint(test.stat, test.raw, "base", "head", test.excluded)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Complete || got.ChangedFiles == nil || *got.ChangedFiles != 1 ||
				len(got.Paths) != 1 || len(got.ManualReasons) == 0 ||
				(got.ChangedLines == nil) != test.unknownLines {
				t.Fatalf("manual footprint = %+v", got)
			}
		})
	}
}

func TestParseMaintenanceFootprintRefusesIncompleteOrInvalidEvidence(t *testing.T) {
	validRaw := footprintRaw("100644", "100644", "M", "file.txt")
	for _, test := range []struct{ name, stat, raw string }{
		{"truncated-stat", "1\t1\tfile.txt", validRaw},
		{"truncated-raw", "1\t1\tfile.txt\x00", strings.TrimSuffix(validRaw, "\x00")},
		{"missing-raw-path", "1\t1\tfile.txt\x00", ""},
		{"extra-raw-path", "", validRaw},
		{"different-path", "1\t1\tother.txt\x00", validRaw},
		{"duplicate-stat", "1\t1\tfile.txt\x001\t1\tfile.txt\x00", validRaw},
		{"duplicate-raw", "1\t1\tfile.txt\x00", validRaw + validRaw},
		{"mixed-binary-counts", "-\t1\tfile.txt\x00", validRaw},
		{"negative-count", "-1\t1\tfile.txt\x00", validRaw},
		{"signed-count", "+1\t1\tfile.txt\x00", validRaw},
		{"fractional-count", "1.5\t1\tfile.txt\x00", validRaw},
		{"overflow-count", "18446744073709551616\t0\tfile.txt\x00", validRaw},
		{"overflow-sum", "18446744073709551615\t1\tfile.txt\x00", validRaw},
		{"unexpected-rename-format", "1\t1\tfile.txt\x00", footprintRaw("100644", "100644", "R100", "file.txt")},
		{"invalid-mode", "1\t1\tfile.txt\x00", footprintRaw("100648", "100644", "M", "file.txt")},
		{"invalid-addition-mode", "1\t0\tfile.txt\x00", footprintRaw("100644", "100644", "A", "file.txt")},
		{"invalid-deletion-mode", "0\t1\tfile.txt\x00", footprintRaw("100644", "100644", "D", "file.txt")},
		{"invalid-modification-mode", "1\t0\tfile.txt\x00", footprintRaw("000000", "100644", "M", "file.txt")},
		{"invalid-object-field", "1\t1\tfile.txt\x00", ":100644 100644 broken 7654321 M\x00file.txt\x00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseMaintenanceFootprint(test.stat, test.raw, "base", "head", nil)
			if err == nil || got.Complete || got.ChangedLines != nil || got.ChangedFiles != nil {
				t.Fatalf("invalid evidence became a numeric authority: %+v, %v", got, err)
			}
		})
	}
}

func TestFootprintRevisionRequiresExactObjectID(t *testing.T) {
	for _, value := range []string{"HEAD", "--stat", "main..branch", strings.Repeat("a", 39), strings.Repeat("g", 40), strings.Repeat("A", 40)} {
		if footprintRevision(value) {
			t.Fatalf("accepted noncanonical revision %q", value)
		}
	}
	for _, value := range []string{strings.Repeat("a", 40), strings.Repeat("0123456789abcdef", 4)} {
		if !footprintRevision(value) {
			t.Fatalf("refused canonical revision %q", value)
		}
	}
}

func TestMaintenanceRemoteFootprintUsesSafeSumAndStricterLimits(t *testing.T) {
	for _, test := range []struct {
		additions, deletions, files uint64
		savedLines, liveLines       uint64
		savedFiles, liveFiles       uint64
		eligible                    bool
	}{
		{250, 250, 10, 500, 500, 10, 10, true},
		{500, 1, 10, 500, 500, 10, 10, false},
		{1, 0, 11, 500, 500, 10, 10, false},
		{501, 0, 10, 500, 1000, 10, 20, false},
		{500, 0, 10, 1000, 499, 20, 10, false},
		{500, 0, 10, 1000, 500, 20, 9, false},
		{math.MaxUint64, 1, 1, 500, 500, 10, 10, false},
		{1, math.MaxUint64, 1, 500, 500, 10, 10, false},
		{0, 0, 0, 500, 500, 10, 10, false},
		{0, 0, 1, 0, 500, 10, 10, false},
		{0, 0, 1, 500, 500, 0, 10, false},
	} {
		saved, live := config.Default(), config.Default()
		saved.AutoMergeMaxLines, live.AutoMergeMaxLines = test.savedLines, test.liveLines
		saved.AutoMergeMaxFiles, live.AutoMergeMaxFiles = test.savedFiles, test.liveFiles
		if got := maintenanceRemoteFootprintWithin(test.additions, test.deletions, test.files, saved, live); got != test.eligible {
			t.Errorf("remote footprint %d+%d / %d, limits %d:%d / %d:%d = %t; want %t",
				test.additions, test.deletions, test.files, test.savedLines, test.liveLines, test.savedFiles, test.liveFiles, got, test.eligible)
		}
	}
}
