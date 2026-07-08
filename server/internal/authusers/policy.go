package authusers

// The central policy function from docs/02-architecture.md section 4 and
// docs/04-api.md section 1: every handler and every WebSocket channel
// subscribe calls Can(actor, action, resource). Ownership and assignment
// checks live here, never scattered in handlers. When Can returns false for
// a specific resource, handlers answer 404 (not 403) so existence is never
// leaked to the unassigned; role gates on whole route groups answer 403.

// Action names one row of the docs/04-api.md permission matrix.
type Action string

const (
	// Admin console.
	ActionUsersManage  Action = "users.manage"  // provision, deactivate, reset credentials
	ActionGroupsManage Action = "groups.manage" // cohorts and membership
	ActionAuditRead    Action = "audit.read"

	// The minimal student-and-group directory teachers read to pick a
	// quiz audience (docs/04-api.md: assignments take student and group
	// ids). It exposes no account status, credentials, or teacher rows.
	ActionDirectoryRead Action = "directory.read"

	// Quiz authoring. Admin cannot author quizzes (docs/08-security.md
	// section 2): authoring is teacher-only, and edit-shaped actions
	// additionally require ownership.
	ActionQuizCreate Action = "quiz.create"
	ActionQuizEdit   Action = "quiz.edit" // questions, imports, publish, assign, extend, close, guardrails

	// Live monitoring and moderation: the owning teacher, or any admin.
	ActionQuizWatchLive   Action = "quiz.watch_live"
	ActionAttemptModerate Action = "attempt.moderate" // kick, readmit

	// Taking a quiz: students only, and only when assigned. The time-window
	// and attempt-count checks are transactional business rules in the
	// attempt module, not identity rules, so they are not here.
	ActionAttemptTake Action = "attempt.take"

	// Analytics: admin sees all; a teacher sees assigned students and
	// themself; a student sees only themself.
	ActionAnalyticsStudent Action = "analytics.student"
	ActionAnalyticsTeacher Action = "analytics.teacher"
	// ActionAnalyticsOrg is the org-wide dashboard (docs/07 section 4, "Org-wide"):
	// admin only, no owner or subject to check.
	ActionAnalyticsOrg Action = "analytics.org"
)

// Resource carries the identity facts Can needs about the target. The caller
// (which loaded the resource anyway) fills in ownership and assignment; the
// zero value describes a global resource with no owner, which is what the
// admin-console actions take.
type Resource struct {
	// OwnerID is who the resource belongs to: the quiz's owning teacher for
	// quiz and attempt actions, or the subject user for analytics actions.
	OwnerID string
	// Assigned reports whether the actor is in the resource's audience: the
	// student is assigned to the quiz, or the analytics subject is one of
	// the teacher's assigned students.
	Assigned bool
}

// Can is the single permission decision for the whole API
// (docs/04-api.md section 1). It is pure: all facts arrive via the actor and
// the resource, so it is trivially unit-testable and can never touch the
// database at subscribe-fan-out rates.
func Can(actor User, action Action, res Resource) bool {
	if actor.Status != "active" {
		return false
	}
	switch action {
	case ActionUsersManage, ActionGroupsManage, ActionAuditRead:
		return actor.Role == "admin"
	case ActionDirectoryRead:
		return actor.Role == "admin" || actor.Role == "teacher"
	case ActionQuizCreate:
		return actor.Role == "teacher"
	case ActionQuizEdit:
		return actor.Role == "teacher" && actor.ID == res.OwnerID
	case ActionQuizWatchLive, ActionAttemptModerate:
		return actor.Role == "admin" ||
			(actor.Role == "teacher" && actor.ID == res.OwnerID)
	case ActionAttemptTake:
		return actor.Role == "student" && res.Assigned
	case ActionAnalyticsStudent:
		switch actor.Role {
		case "admin":
			return true
		case "teacher":
			return res.Assigned
		case "student":
			return actor.ID == res.OwnerID
		}
		return false
	case ActionAnalyticsTeacher:
		return actor.Role == "admin" ||
			(actor.Role == "teacher" && actor.ID == res.OwnerID)
	case ActionAnalyticsOrg:
		return actor.Role == "admin"
	}
	// Unknown actions are denied: a typo can only ever fail closed.
	return false
}
