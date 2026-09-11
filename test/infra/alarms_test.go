package infra

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every alarm bound to the application's metric namespace names an
// instrument the Go code constructs.
//
// The two lists live in different languages in different directories, and a
// list duplicated in two places diverges. F-118 was the extreme case: the
// instruments existed as names in Terraform and as constructors in Go, and
// for a year nothing constructed them, so the alarms would have read green
// over a system emitting nothing. This cannot catch "constructed but never
// called"; it catches "renamed on one side", which is the next way the same
// wall of green comes back.
func TestEveryApplicationAlarmNamesAnInstrumentThatExists(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	tf, err := os.ReadFile(filepath.Join(root, "infra", "terraform", "modules", "observability", "main.tf"))
	require.NoError(t, err)

	// An instrument name is the first argument to a constructor -- the
	// builder in internal/observability or the OTel meter directly -- or the
	// value of a metricXxx constant, which is how cmd/relay-worker passes its
	// names through a table. Anything else is a string that happens to be
	// snake_case.
	instruments := map[string]bool{}
	nameRes := []*regexp.Regexp{
		regexp.MustCompile(`\bb\.(?:counter|gauge|histogram|updown)\("([a-z][a-z0-9_]*)"`),
		regexp.MustCompile(`meter\.(?:Int64|Float64)(?:Observable)?(?:Counter|UpDownCounter|Gauge|Histogram)\("([a-z][a-z0-9_]*)"`),
		regexp.MustCompile(`\bmetric[A-Z]\w*\s*=\s*"([a-z][a-z0-9_]*)"`),
	}
	for _, dir := range []string{"internal", "cmd"} {
		require.NoError(t, filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, re := range nameRes {
				for _, m := range re.FindAllStringSubmatch(string(src), -1) {
					instruments[m[1]] = true
				}
			}
			return nil
		}))
	}
	require.NotEmpty(t, instruments, "no instrument constructors found; the regexps are stale")
	// The scan reaches both construction styles, or the assertion below is
	// weaker than it looks.
	require.True(t, instruments["ledger_posting_errors"], "the builder style was not seen")
	require.True(t, instruments["outbox_publish_failures"], "the relay-worker constant style was not seen")

	metricRe := regexp.MustCompile(`metric_name\s*=\s*"([A-Za-z0-9_]+)"`)
	var missing []string
	blocks := strings.Split(string(tf), `resource "aws_cloudwatch_metric_alarm"`)
	var heartbeat string
	for _, b := range blocks[1:] {
		if strings.Contains(b, `"verification_passes"`) {
			heartbeat = b
		}
		if !strings.Contains(b, "var.custom_metric_namespace") {
			continue // an AWS-namespace alarm names an AWS metric
		}
		for _, m := range metricRe.FindAllStringSubmatch(b, -1) {
			if !instruments[m[1]] {
				missing = append(missing, m[1])
			}
		}
	}
	sort.Strings(missing)
	require.Empty(t, missing, "alarms bound to the application namespace name metrics no Go instrument emits; they will sit in INSUFFICIENT_DATA (or read OK) forever")

	// And the heartbeat specifically: the one alarm whose whole purpose is
	// to fire when the others cannot.
	require.NotEmpty(t, heartbeat, "no alarm watches verification_passes; a system emitting nothing reads green again")
	require.Contains(t, heartbeat, `treat_missing_data  = "breaching"`,
		"the heartbeat alarm treats a missing sample as fine, which is the one thing a heartbeat must not do")
}
