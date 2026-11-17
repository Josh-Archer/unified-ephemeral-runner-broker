package localvmintent

import (
	"strings"
	"testing"
)

func candidate() Candidate {
	return Candidate{Repository: "Josh-Archer/DeskQuest", RunID: 1, JobID: 2, Attempt: 1, HeadSHA: "1111111111111111111111111111111111111111", CheckoutSHA: "2222222222222222222222222222222222222222", WorkflowSHA: "3333333333333333333333333333333333333333", WorkflowPath: ".github/workflows/compositors.yml"}
}
func TestRejectIncompleteOrMalformedCandidate(t *testing.T) {
	cases := []func(*Candidate){
		func(c *Candidate) { c.Repository = "" }, func(c *Candidate) { c.Repository = "a/b/c" },
		func(c *Candidate) { c.RunID = 0 }, func(c *Candidate) { c.JobID = -1 }, func(c *Candidate) { c.Attempt = 0 },
		func(c *Candidate) { c.HeadSHA = "main" }, func(c *Candidate) { c.CheckoutSHA = "" }, func(c *Candidate) { c.WorkflowSHA = "HEAD" },
		func(c *Candidate) { c.WorkflowPath = ".github/workflows/../unsafe.yml" },
	}
	for n, change := range cases {
		c := candidate()
		change(&c)
		if _, err := New(c); err != ErrInvalid {
			t.Fatalf("case %d accepted", n)
		}
	}
	if _, err := (Intent{}).Candidate(); err != ErrInvalid {
		t.Fatal("zero intent accepted")
	}
}
func TestImmutableCandidatePreservesDistinctRevisions(t *testing.T) {
	c := candidate()
	i, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	c.HeadSHA = "changed"
	got, err := i.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	if got != candidate() {
		t.Fatal("candidate mutated or revision collapsed")
	}
	got.Repository = "other/repository"
	again, _ := i.Candidate()
	if again != candidate() {
		t.Fatal("accessor exposed mutable state")
	}
}

func TestRepositorySyntax(t *testing.T) {
	for _, name := range []string{"../..", "./repo", "owner/.", "owner/..", "owner/", "/repo", "a/b/c", "a_b/repo", "a.b/repo", "-owner/repo", "owner-/repo", "own--er/repo", "owner/repo name", "owner/repo%2fother", strings.Repeat("a", 40) + "/repo", "owner/" + strings.Repeat("a", 101)} {
		c := candidate()
		c.Repository = name
		if _, err := New(c); err != ErrInvalid {
			t.Errorf("accepted %q", name)
		}
	}
	for _, name := range []string{"Josh-Archer/DeskQuest", "a/.github", "Owner/_repo", "Owner/repo.name-1", "owner/-", "owner/...", strings.Repeat("a", 39) + "/" + strings.Repeat("b", 100)} {
		c := candidate()
		c.Repository = name
		i, err := New(c)
		if err != nil {
			t.Errorf("rejected %q", name)
			continue
		}
		got, _ := i.Candidate()
		if got.Repository != name {
			t.Errorf("normalized %q", name)
		}
	}
}
