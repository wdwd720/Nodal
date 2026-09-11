package agents

import (
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/errs"
)

// AuthorityDescription is the product's own words for one authority level: the
// four distinctions goal §17 requires a user to be able to tell apart, plus the
// disabled rungs above them, rendered from the matrix internal/agentauthority
// enforces rather than written out a second time beside it.
type AuthorityDescription struct {
	// Level is the numeric level (agentauthority.Level).
	Level int `json:"level"`
	// Name is the level's stable name, e.g. "USER_APPROVED_RULE".
	Name string `json:"name"`
	// Summary is one sentence a person can act on. It never implies
	// unrestricted autonomous financial authority (goal §17).
	Summary string `json:"summary"`
	// Enabled reports whether this build permits the level at all.
	Enabled bool `json:"enabled"`
	// RequiredCapability names the gate the level would need. Empty for the
	// levels this build supports.
	RequiredCapability string `json:"required_capability,omitempty"`
}

// levelSummaries are the user-facing sentences. They are deliberately about
// WHO DECIDES, because that is the distinction the goal asks a user to be able
// to make, and it is the one a wording like "smart" or "automated" destroys.
var levelSummaries = map[agentauthority.Level]string{
	agentauthority.LevelResearchOnly: "Research only: it reads approved data and writes analysis. " +
		"It cannot propose an order and cannot touch your Credits.",
	agentauthority.LevelRecommendation: "Recommendation: it proposes; you decide every time. " +
		"Nothing is prepared or sent until you act on it yourself.",
	agentauthority.LevelPrepareTransaction: "Prepared action: it builds an order and stops. " +
		"You confirm each one before anything is submitted.",
	agentauthority.LevelUserApprovedRule: "Rule-based execution within the limits you set: it runs the exact rule " +
		"you read and approved, inside the Credit budget, per-trade cap, loss stop and asset list on this page. " +
		"It never chooses an asset you did not list and never raises its own limits.",
	agentauthority.LevelBoundedDiscretion: "Bounded discretion: it would choose among options you pre-approved. " +
		"Disabled by policy in this deployment.",
	agentauthority.LevelAutonomousSelection: "Autonomous selection: it would choose investments itself. " +
		"Disabled by policy in this deployment.",
	agentauthority.LevelAutonomousPortfolio: "Autonomous portfolio: it would allocate across strategies and assets. " +
		"Disabled by policy in this deployment.",
}

// AuthorityLevels renders every declared level, enabled or not.
//
// The disabled ones are returned rather than hidden: a product that silently
// omits levels 4-6 teaches nobody that they exist and are switched off, and the
// day one is approved the UI would have to learn about it. The API says
// "declared, disabled, and here is the gate it would need".
func AuthorityLevels() []AuthorityDescription {
	levels := agentauthority.AllLevels()
	out := make([]AuthorityDescription, 0, len(levels))
	for _, l := range levels {
		out = append(out, AuthorityDescription{
			Level:              int(l),
			Name:               l.Name(),
			Summary:            levelSummaries[l],
			Enabled:            l.SupportedInThisBuild(),
			RequiredCapability: string(l.RequiresCapability()),
		})
	}
	return out
}

// ParseAuthorityLevel accepts a level a user asked for and refuses everything
// this build does not implement.
//
// The refusal is deliberately not NOT_FOUND or VALIDATION_FAILED alone: a
// caller asking for level 5 has not made a typing mistake, they have asked for
// an authority that exists in the architecture and is switched off. They are
// told which capability would have to be approved, so the answer is checkable
// rather than a shrug.
func ParseAuthorityLevel(level int) (agentauthority.Level, error) {
	l := agentauthority.Level(level)
	if !l.Valid() {
		return 0, errs.Newf(errs.CodeValidationFailed, "agents: %d is not a declared authority level", level).
			WithField("authority_level", level)
	}
	if !l.SupportedInThisBuild() {
		e := errs.Newf(errs.CodeCapabilityNotApproved,
			"agents: authority level %s is disabled by policy in this deployment and cannot be granted", l.String()).
			WithField("authority_level", level).
			WithField("max_supported_level", int(agentauthority.MaxSupportedLevel))
		if c := l.RequiresCapability(); c != "" {
			e = e.WithField("required_capability", string(c))
		}
		return 0, e
	}
	return l, nil
}

// ExecutesWithoutConfirmation reports whether a level lets the agent act
// without a person confirming that specific action. Only level 3 does, and it
// is the level whose enablement this package gates on a capability.
func ExecutesWithoutConfirmation(l agentauthority.Level) bool {
	return l >= agentauthority.LevelUserApprovedRule
}
