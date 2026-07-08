package authusers

import "testing"

// TestCan pins the whole docs/04-api.md permission matrix, one case per cell
// that matters. The policy is pure, so this table is the executable spec.
func TestCan(t *testing.T) {
	admin := User{ID: "a1", Role: "admin", Status: "active"}
	teacher := User{ID: "t1", Role: "teacher", Status: "active"}
	student := User{ID: "s1", Role: "student", Status: "active"}

	ownQuiz := Resource{OwnerID: "t1"}
	otherQuiz := Resource{OwnerID: "t2"}

	cases := []struct {
		name   string
		actor  User
		action Action
		res    Resource
		want   bool
	}{
		{"admin manages users", admin, ActionUsersManage, Resource{}, true},
		{"teacher cannot manage users", teacher, ActionUsersManage, Resource{}, false},
		{"student cannot manage users", student, ActionUsersManage, Resource{}, false},
		{"admin manages groups", admin, ActionGroupsManage, Resource{}, true},
		{"teacher cannot manage groups", teacher, ActionGroupsManage, Resource{}, false},
		{"admin reads audit log", admin, ActionAuditRead, Resource{}, true},
		{"teacher cannot read audit log", teacher, ActionAuditRead, Resource{}, false},

		// The audience-picker directory is readable by quiz authors and
		// admins, never by students.
		{"teacher reads directory", teacher, ActionDirectoryRead, Resource{}, true},
		{"admin reads directory", admin, ActionDirectoryRead, Resource{}, true},
		{"student cannot read directory", student, ActionDirectoryRead, Resource{}, false},

		// Admin cannot author quizzes (docs/08-security.md section 2).
		{"teacher creates quiz", teacher, ActionQuizCreate, Resource{}, true},
		{"admin cannot create quiz", admin, ActionQuizCreate, Resource{}, false},
		{"student cannot create quiz", student, ActionQuizCreate, Resource{}, false},
		{"teacher edits own quiz", teacher, ActionQuizEdit, ownQuiz, true},
		{"teacher cannot edit another's quiz", teacher, ActionQuizEdit, otherQuiz, false},
		{"admin cannot edit any quiz", admin, ActionQuizEdit, ownQuiz, false},

		{"owner watches own live quiz", teacher, ActionQuizWatchLive, ownQuiz, true},
		{"teacher cannot watch another's live quiz", teacher, ActionQuizWatchLive, otherQuiz, false},
		{"admin watches any live quiz", admin, ActionQuizWatchLive, otherQuiz, true},
		{"owner kicks from own quiz", teacher, ActionAttemptModerate, ownQuiz, true},
		{"teacher cannot kick from another's quiz", teacher, ActionAttemptModerate, otherQuiz, false},
		{"admin kicks from any quiz", admin, ActionAttemptModerate, otherQuiz, true},
		{"student cannot moderate", student, ActionAttemptModerate, ownQuiz, false},

		{"assigned student takes quiz", student, ActionAttemptTake, Resource{OwnerID: "t1", Assigned: true}, true},
		{"unassigned student cannot take quiz", student, ActionAttemptTake, ownQuiz, false},
		{"teacher cannot take quiz", teacher, ActionAttemptTake, Resource{OwnerID: "t2", Assigned: true}, false},

		{"admin sees any student analytics", admin, ActionAnalyticsStudent, Resource{OwnerID: "s1"}, true},
		{"teacher sees assigned student analytics", teacher, ActionAnalyticsStudent, Resource{OwnerID: "s1", Assigned: true}, true},
		{"teacher cannot see unassigned student analytics", teacher, ActionAnalyticsStudent, Resource{OwnerID: "s1"}, false},
		{"student sees own analytics", student, ActionAnalyticsStudent, Resource{OwnerID: "s1"}, true},
		{"student cannot see another's analytics", student, ActionAnalyticsStudent, Resource{OwnerID: "s2"}, false},
		{"admin sees any teacher analytics", admin, ActionAnalyticsTeacher, Resource{OwnerID: "t1"}, true},
		{"teacher sees own analytics", teacher, ActionAnalyticsTeacher, Resource{OwnerID: "t1"}, true},
		{"teacher cannot see another teacher's analytics", teacher, ActionAnalyticsTeacher, Resource{OwnerID: "t2"}, false},
		{"student cannot see teacher analytics", student, ActionAnalyticsTeacher, Resource{OwnerID: "t1"}, false},

		{"admin reads org analytics", admin, ActionAnalyticsOrg, Resource{}, true},
		{"teacher cannot read org analytics", teacher, ActionAnalyticsOrg, Resource{}, false},
		{"student cannot read org analytics", student, ActionAnalyticsOrg, Resource{}, false},

		{"disabled admin can do nothing", User{ID: "a1", Role: "admin", Status: "disabled"}, ActionUsersManage, Resource{}, false},
		{"unknown action fails closed", admin, Action("typo.action"), Resource{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Can(tc.actor, tc.action, tc.res); got != tc.want {
				t.Fatalf("Can(%s %s, %q, %+v) = %v, want %v",
					tc.actor.Role, tc.actor.ID, tc.action, tc.res, got, tc.want)
			}
		})
	}
}
