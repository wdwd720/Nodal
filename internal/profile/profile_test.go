package profile

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

func TestValidateDisplayName_Accepts(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Ada":                "Ada",
		"  Ada Lovelace  ":   "Ada Lovelace",
		"Ada\t\tLovelace":    "Ada Lovelace",
		"Ada  Lovelace":      "Ada Lovelace",
		"Ada Lovelace (she)": "Ada Lovelace (she)",
		// A newline is whitespace: it is collapsed, not smuggled through. What
		// the rules refuse is a character that makes the RENDERED string differ
		// from its code points, which a space does not.
		"Ada\nLovelace":         "Ada Lovelace",
		"李雷":                    "李雷",
		"x2":                    "x2",
		strings.Repeat("a", 64): strings.Repeat("a", 64),
	} {
		got, err := ValidateDisplayName(in)
		require.NoErrorf(t, err, "input %q", in)
		assert.Equal(t, want, got)
	}
}

func TestValidateDisplayName_Refuses(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"empty":                    "",
		"whitespace only":          "   ",
		"too long":                 strings.Repeat("a", 65),
		"no letter or digit":       "***",
		"a control character":      "Ada\x07Lovelace",
		"a zero-width space":       "Ada\u200bLovelace",
		"a zero-width joiner":      "Ada\u200dLovelace",
		"a right-to-left override": "Ada\u202eLovelace",
		"a left-to-right isolate":  "Ada\u2066Lovelace",
		"a soft hyphen":            "Ada\u00adLovelace",
		"a byte-order mark":        "\ufeffAda",
		"a word joiner":            "Ada\u2060Lovelace",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateDisplayName(in)
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// A rune count, not a byte count: 64 two-byte characters is 64 characters.
func TestValidateDisplayName_BoundsRunesNotBytes(t *testing.T) {
	t.Parallel()
	_, err := ValidateDisplayName(strings.Repeat("é", 64))
	assert.NoError(t, err)
	_, err = ValidateDisplayName(strings.Repeat("é", 65))
	assert.Error(t, err)
}

func TestValidateHandle_AcceptsAndLowerCases(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"ada":                   "ada",
		"Ada":                   "ada",
		"  AdaLovelace  ":       "adalovelace",
		"ada_l1":                "ada_l1",
		strings.Repeat("a", 30): strings.Repeat("a", 30),
	} {
		got, err := ValidateHandle(in)
		require.NoErrorf(t, err, "input %q", in)
		assert.Equal(t, want, got)
	}
}

func TestValidateHandle_Refuses(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"empty":                  "",
		"too short":              "ab",
		"too long":               strings.Repeat("a", 31),
		"starts with a digit":    "1ada",
		"starts with underscore": "_ada",
		"a hyphen":               "ada-l",
		"a space":                "ada l",
		"a dot":                  "ada.l",
		"a reserved word":        "admin",
		"a reserved route":       "settings",
		"a reserved claim":       "verified",
		"reserved in mixed case": "Support",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateHandle(in)
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// Every reserved word must itself be a syntactically valid handle. A reserved
// list containing entries the pattern already refuses is a list whose entries
// were never doing anything.
func TestReservedHandles_AreAllShapesAHandleCouldOtherwiseTake(t *testing.T) {
	t.Parallel()
	reserved := ReservedHandles()
	require.NotEmpty(t, reserved)
	for _, h := range reserved {
		assert.Truef(t, handlePattern.MatchString(h),
			"%q is reserved but the handle pattern already refuses it, so reserving it does nothing", h)
	}
	for i := 1; i < len(reserved); i++ {
		assert.Less(t, reserved[i-1], reserved[i], "ReservedHandles is not sorted")
	}
}

func TestValidateLocale(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"en", "en-GB", "fr", "pt-BR"} {
		got, err := ValidateLocale(ok)
		require.NoErrorf(t, err, "input %q", ok)
		assert.Equal(t, ok, got)
	}
	for _, bad := range []string{"", "e", "eng", "EN", "en-gb", "en_GB", "en-GBR", "../../etc"} {
		_, err := ValidateLocale(bad)
		assert.Errorf(t, err, "input %q was accepted", bad)
	}
}

func TestValidateTimeZone(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"UTC", "Europe/London", "America/New_York", "America/Argentina/Ushuaia", "Etc/GMT+3"} {
		got, err := ValidateTimeZone(ok)
		require.NoErrorf(t, err, "input %q", ok)
		assert.Equal(t, ok, got)
	}
	for _, bad := range []string{"", "London", "Europe/", "/London", "Europe//London", "../../etc/passwd", strings.Repeat("a", 65) + "/x"} {
		_, err := ValidateTimeZone(bad)
		assert.Errorf(t, err, "input %q was accepted", bad)
	}
}

// The seed is stable for a user, differs between users, and is not the user id.
func TestAvatarSeedFor(t *testing.T) {
	t.Parallel()
	const a = "0193b2e0-0000-7000-8000-000000000001"
	const b = "0193b2e0-0000-7000-8000-000000000002"
	assert.Equal(t, AvatarSeedFor(a), AvatarSeedFor(a))
	assert.NotEqual(t, AvatarSeedFor(a), AvatarSeedFor(b))
	assert.Len(t, AvatarSeedFor(a), 16)
	assert.NotContains(t, AvatarSeedFor(a), strings.ReplaceAll(a, "-", "")[:8])
}

func TestOnboarding_StepsAndNextStep(t *testing.T) {
	t.Parallel()
	var o Onboarding
	assert.False(t, o.Complete())
	assert.Equal(t, StepProfile, o.NextStep())
	require.Len(t, o.Steps(), len(Steps()))

	now := testTime()
	o.DisplayNameSetAt = &now
	assert.Equal(t, StepTerms, o.NextStep())

	o.TermsAcceptedAt = &now
	assert.Equal(t, StepKey(""), o.NextStep())
	assert.False(t, o.Complete(), "the checklist being done is not the same fact as the stamp being written")

	o.CompletedAt = &now
	assert.True(t, o.Complete())
}

func TestPatch_EmptyAndChangedFields(t *testing.T) {
	t.Parallel()
	assert.True(t, Patch{}.Empty())
	name := "Ada"
	assert.False(t, Patch{DisplayName: &name}.Empty())
	assert.Equal(t, []string{"display_name"}, changedFields(Patch{DisplayName: &name}))
}

func TestNormalizePatch_ValidatesAndReportsChange(t *testing.T) {
	t.Parallel()
	name, handle := "  Ada  ", "AdaL"
	got, changed, err := normalizePatch(Patch{DisplayName: &name, Handle: &handle})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "Ada", *got.DisplayName)
	assert.Equal(t, "adal", *got.Handle)

	// The empty handle clears rather than failing validation.
	empty := ""
	got, changed, err = normalizePatch(Patch{Handle: &empty})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "", *got.Handle)

	bad := "!!"
	_, _, err = normalizePatch(Patch{DisplayName: &bad})
	require.Error(t, err)

	_, changed, err = normalizePatch(Patch{})
	require.NoError(t, err)
	assert.False(t, changed)
}
