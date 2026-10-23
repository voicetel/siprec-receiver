package store

import (
	"context"
	"testing"
)

// Only SetUserDisabled changes a user's disabled flag: saving their
// role (the add-user form, which never sends the flag) keeps it.
func TestUserDisabledSurvivesARoleChange(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertUser(ctx, User{Email: "v@example.com", Role: "user"}); err != nil {
		t.Fatal(err)
	}

	if found, err := s.SetUserDisabled(ctx, "V@Example.com", true); !found || err != nil {
		t.Fatalf("disable = %v, %v", found, err)
	}

	if err := s.UpsertUser(ctx, User{Email: "v@example.com", Role: "admin"}); err != nil {
		t.Fatal(err)
	}

	if u, _ := s.GetUser(ctx, "v@example.com"); !u.Disabled || u.Role != "admin" {
		t.Errorf("after the role change: disabled %v role %q", u.Disabled, u.Role)
	}

	if found, err := s.SetUserDisabled(ctx, "v@example.com", false); !found || err != nil {
		t.Fatalf("enable = %v, %v", found, err)
	}

	if u, _ := s.GetUser(ctx, "v@example.com"); u.Disabled {
		t.Error("still disabled after enable")
	}

	if found, err := s.SetUserDisabled(ctx, "nobody@example.com", true); found || err != nil {
		t.Errorf("absent user = %v, %v", found, err)
	}

	_ = s.Close()

	_, err := s.SetUserDisabled(ctx, "v@example.com", true)
	wantErr(t, err, "set user disabled")
}
