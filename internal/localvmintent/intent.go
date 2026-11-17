// Package localvmintent defines an inert candidate binding for a future local VM lane.
// Validation is syntax checking, never authorization or permission to launch.
package localvmintent

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

var ErrInvalid = errors.New("invalid local VM candidate binding")
var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Candidate is untrusted input, including when all its fields are valid.
// HeadSHA and CheckoutSHA are separate: a PR merge checkout is not its head.
type Candidate struct {
	Repository                        string
	RunID, JobID, Attempt             int64
	HeadSHA, CheckoutSHA, WorkflowSHA string
	WorkflowPath                      string
}

// Intent is an immutable, syntactically validated candidate, NOT an admission.
// The zero value is invalid. No credentials, labels, or launch settings are held.
type Intent struct {
	candidate Candidate
	valid     bool
}

func New(c Candidate) (Intent, error) {
	if !repository(c.Repository) || c.RunID <= 0 || c.JobID <= 0 || c.Attempt <= 0 ||
		!shaPattern.MatchString(c.HeadSHA) || !shaPattern.MatchString(c.CheckoutSHA) || !shaPattern.MatchString(c.WorkflowSHA) ||
		!workflowPath(c.WorkflowPath) {
		return Intent{}, ErrInvalid
	}
	return Intent{candidate: c, valid: true}, nil
}

// Check identifier syntax without normalizing case or treating it as existence proof.
func repository(value string) bool {
	owner, repo, ok := strings.Cut(value, "/")
	return ok && len(owner) <= 39 && ownerPattern.MatchString(owner) &&
		len(repo) <= 100 && repoPattern.MatchString(repo) && repo != "." && repo != ".."
}

func workflowPath(p string) bool {
	return regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_-]+\.ya?ml$`).MatchString(p)
}

// Candidate returns a value copy. A valid value remains untrusted.
func (i Intent) Candidate() (Candidate, error) {
	if !i.valid {
		return Candidate{}, ErrInvalid
	}
	return i.candidate, nil
}

// EvidenceVerifier is an integration boundary, not an implementation or admission.
// Implementations MUST independently retrieve authenticated server evidence and
// verify repository/run/job/attempt ownership, head versus checkout SHA semantics,
// workflow path/revision and policy approval. Caller metadata and labels cannot
// establish trust. A nil error alone MUST NOT authorize runner registration or VM
// launch: durable fencing and job-assignment isolation remain separate blockers.
// There is deliberately no production implementation or caller in this package.
type EvidenceVerifier interface {
	VerifyCandidate(context.Context, Intent) error
}
