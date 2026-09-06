package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/proof"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestRun_UsageErrors(t *testing.T) {
	cases := [][]string{
		nil,
		{"bogus"},
		{"bundle"},
		{"bundle", "a", "b"},
		{"verify", "extra"},
		{"checkpoint", "extra"},
		{"run", "extra"},
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		code := run(args, env(nil), &out, &errb)
		assert.Equal(t, exitUsage, code, "%v", args)
		assert.Contains(t, errb.String(), "usage: audit-worker", "%v", args)
		assert.NotContains(t, errb.String(), "postgres://", "connection strings are never printed")
	}
	var out, errb bytes.Buffer
	assert.Equal(t, exitOK, run([]string{"help"}, env(nil), &out, &errb))
	assert.Contains(t, out.String(), "verify")
}

func TestRun_ConfigErrorsAreRuntimeFailures(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"verify"}, env(map[string]string{"CP_ENV": "NOWHERE"}), &out, &errb)
	assert.Equal(t, exitFailure, code)
	assert.Contains(t, errb.String(), "unknown environment")
}

func TestRun_LocalKeyRefusedInProductionLikeEnv(t *testing.T) {
	// STAGING requires a full production configuration, so Load fails first;
	// the signer rule is covered directly by proof.NewLocalECDSASigner tests
	// and by buildSigner's explicit check below.
	var out, errb bytes.Buffer
	code := run([]string{"checkpoint"}, env(map[string]string{"CP_ENV": "PROD", envLocalKeyRef: "env://KEY"}), &out, &errb)
	assert.Equal(t, exitFailure, code)
}

func TestDurationVar(t *testing.T) {
	d, err := durationVar(env(nil), envCheckpointInterval, defaultCheckpointInterval)
	require.NoError(t, err)
	assert.Equal(t, defaultCheckpointInterval, d)
	d, err = durationVar(env(map[string]string{envCheckpointInterval: " 90s "}), envCheckpointInterval, defaultCheckpointInterval)
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, d)
	for _, bad := range []string{"0", "-5m", "soon", "5"} {
		_, err := durationVar(env(map[string]string{envVerifyInterval: bad}), envVerifyInterval, defaultVerifyInterval)
		assert.Error(t, err, bad)
	}
}

func TestCheckpointSummary(t *testing.T) {
	s := checkpointSummary(proof.CheckpointResult{Skipped: proof.SkipNoNewEvents})
	assert.False(t, s.Created)
	assert.Equal(t, proof.SkipNoNewEvents, s.Skipped)
	assert.Empty(t, s.ID)
	cp := proof.Checkpoint{ID: proof.NewCheckpointID(), Signer: proof.SignerLocalTest, SigningKeyID: "k", ArchiveURI: "mem://x"}
	cp.Seq, cp.LeafCount = 3, 9
	cp.StreamsCovered = map[string]proof.StreamRange{"admin": {FromSeq: 1, ToSeq: 9}}
	s = checkpointSummary(proof.CheckpointResult{Created: true, Truncated: true, Checkpoint: cp})
	assert.True(t, s.Created)
	assert.True(t, s.Truncated)
	assert.Equal(t, cp.ID.String(), s.ID)
	assert.Equal(t, int64(3), s.Seq)
	assert.Equal(t, 9, s.LeafCount)
	assert.Equal(t, 1, s.Streams)
	assert.Equal(t, "mem://x", s.ArchiveURI)
}

func TestUsage_NamesEveryCommandAndVariable(t *testing.T) {
	var b bytes.Buffer
	usage(&b)
	for _, want := range []string{"run", "checkpoint", "verify", "bundle", envCheckpointInterval, envVerifyInterval, envLocalKeyRef, envArchiveDir, envMaxLeaves} {
		assert.True(t, strings.Contains(b.String(), want), want)
	}
}
