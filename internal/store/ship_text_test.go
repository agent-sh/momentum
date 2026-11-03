package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestShipTransitionsCleanBeforeOpeningTransaction(t *testing.T) {
	for _, action := range []string{"deploy", "rollback"} {
		for _, fail := range []bool{false, true} {
			t.Run(action+map[bool]string{false: "/clean", true: "/refused"}[fail], func(t *testing.T) {
				st := caseStore(t)
				ctx := context.Background()
				c, _ := observeOK(t, st, Finding{Family: "failures", Signature: "marker-7f3a", Title: "One cause",
					Evidence: refsAt("marker-7f3a", 1, time.Now())})
				mechanism, fix := "One cause", "One test"
				c, err := st.UpdateCase(ctx, c.ID, CaseUpdate{Status: CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture")
				if err != nil {
					t.Fatal(err)
				}
				if action == "rollback" {
					c, err = st.DeployCase(ctx, c.ID, "fixture", time.Now(), "prepared", "fixture")
					if err != nil {
						t.Fatal(err)
					}
				}
				before, err := st.CaseLog(ctx, c.ID)
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				blocked := errors.New("marker-7f3a cleaner refused")
				cleanCtx := WithTextCleaner(ctx, func(_ context.Context, text string) (string, error) {
					calls++
					if st.db.Stats().InUse != 0 {
						t.Error("cleaner holds a transaction connection")
					}
					if fail {
						return "", blocked
					}
					return strings.ReplaceAll(text, "marker-7f3a", "[masked]"), nil
				})
				if action == "deploy" {
					_, err = st.DeployCase(cleanCtx, c.ID, "fixture", time.Now(), "marker-7f3a", "fixture")
				} else {
					_, err = st.RollBackCase(cleanCtx, c.ID, "marker-7f3a", "fixture")
				}
				if calls != 1 || (fail && !errors.Is(err, blocked)) || (!fail && err != nil) {
					t.Fatalf("cleaner or transition: calls=%d %v", calls, err)
				}
				after, err := st.CaseLog(ctx, c.ID)
				if err != nil {
					t.Fatal(err)
				}
				if fail {
					current, err := st.CaseByID(ctx, c.ID)
					if err != nil || current.Status != c.Status || len(after) != len(before) {
						t.Fatal("failed cleaner changed the case", err)
					}
				} else if len(after) != len(before)+1 || after[len(after)-1].Detail != "[masked]" {
					t.Fatal("transition log lacks cleaned text")
				}
			})
		}
	}
}
