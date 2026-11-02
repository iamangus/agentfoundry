package api

import (
	"testing"

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/store"
)

func TestProfilePermissions(t *testing.T) {
	for _, tc := range []struct {
		scope, owner, team string
		ac                 *auth.AuthContext
		visible, editable  bool
	}{
		{"user", "alice", "", nil, false, false},
		{"user", "alice", "", &auth.AuthContext{Subject: "alice"}, true, true},
		{"user", "alice", "", &auth.AuthContext{Subject: "bob"}, false, false},
		{"global", "alice", "", &auth.AuthContext{Subject: "bob"}, true, false},
		{"global", "alice", "", &auth.AuthContext{IsGlobalAdmin: true}, true, true},
		{"team", "alice", "eng", &auth.AuthContext{Subject: "bob", Teams: []string{"eng"}}, true, false},
		{"team", "alice", "eng", &auth.AuthContext{Subject: "bob", Teams: []string{"eng"}, IsTeamAdmin: true}, true, true},
		{"team", "alice", "eng", &auth.AuthContext{Subject: "bob", Teams: []string{"other"}, IsTeamAdmin: true}, false, false},
	} {
		p := &store.InferenceProfile{Scope: tc.scope, CreatedBy: tc.owner, Team: tc.team}
		if profileVisible(p, tc.ac) != tc.visible || profileEditable(p, tc.ac) != tc.editable {
			t.Errorf("unexpected permissions for %+v %+v", p, tc.ac)
		}
	}
}
