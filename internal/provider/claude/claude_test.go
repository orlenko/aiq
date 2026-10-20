package claude

import (
	"errors"
	"testing"
)

func TestParseAuthStatus(t *testing.T) {
	exit1 := errors.New("exit status 1")
	cases := []struct {
		name     string
		out      string
		runErr   error
		email    string
		loggedIn bool
		wantErr  bool
	}{
		{"logged in", `{"loggedIn": true, "email": "a@example.com", "subscriptionType": "max"}`, nil, "a@example.com", true, false},
		// A logged-out home exits 1 and still prints its state.
		{"logged out", `{"loggedIn": false, "authMethod": "none"}`, exit1, "", false, false},
		{"noise before JSON", "warning: something\n{\"loggedIn\": true, \"email\": \"b@example.com\"}", nil, "b@example.com", true, false},
		{"crash", "TypeError: boom", exit1, "", false, true},
		{"no login field", `{"email": "c@example.com"}`, nil, "", false, true},
	}
	for _, c := range cases {
		email, loggedIn, err := parseAuthStatus([]byte(c.out), c.runErr)
		if (err != nil) != c.wantErr || email != c.email || loggedIn != c.loggedIn {
			t.Errorf("%s: got (%q, %v, %v), want (%q, %v, err=%v)", c.name, email, loggedIn, err, c.email, c.loggedIn, c.wantErr)
		}
	}
}
