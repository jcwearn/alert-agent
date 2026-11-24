// Package proposal turns a patch the agent proposes into a commit on an
// agent/* branch of the GitOps repository. It is the controller's half of the
// PR path: Claude never holds a write credential, and everything it hands
// over is checked here first.
package proposal

import (
	"fmt"
	"regexp"
	"strings"
)

// Rejected is a proposal the controller refused. Its message goes back to
// the agent, which can fix the patch and try again.
type Rejected struct{ Reason string }

func (r *Rejected) Error() string { return r.Reason }

func reject(format string, args ...any) error {
	return &Rejected{Reason: fmt.Sprintf(format, args...)}
}

// allowedPrefixes are the only trees a proposal may touch: the manifests
// Flux applies.
var allowedPrefixes = []string{"apps/", "infrastructure/"}

// deniedPrefixes are refused even inside the allowed trees. apps/alert-agent
// is the agent's own Deployment, RBAC and NetworkPolicy: it must never
// propose changes to its own permissions.
var deniedPrefixes = []string{"apps/alert-agent/"}

// checkPath refuses anything outside the allowed trees. The paths outside --
// clusters/, .github/, .githooks/, scripts/, .claude/, docs/plans/, the SOPS
// config -- are exactly the ones that steer Flux, CI, the publish gate or the
// hooks this package relies on. scripts/ matters most: the pre-commit hook
// runs scripts/render.sh from the commit being checked.
func checkPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains("/"+p+"/", "/../") || strings.Contains("/"+p+"/", "/./") {
		return reject("path %q is not a plain repository path", p)
	}
	if strings.HasSuffix(p, ".sops.yaml") || strings.HasSuffix(p, ".sops.yml") {
		return reject("%s: encrypted secrets are never changed by the agent", p)
	}
	for _, d := range deniedPrefixes {
		if strings.HasPrefix(p, d) {
			return reject("%s: the agent may not change its own deployment", p)
		}
	}
	for _, a := range allowedPrefixes {
		if strings.HasPrefix(p, a) {
			return nil
		}
	}
	return reject("%s: only files under %s may be changed", p, strings.Join(allowedPrefixes, " or "))
}

// change is one entry of `git diff --cached --raw`.
type change struct {
	OldMode, NewMode string
	Status           string
	Path             string
}

// parseRaw reads `git diff --cached --raw -z --no-renames` output:
// ":<old mode> <new mode> <old sha> <new sha> <status>\0<path>\0" per file.
func parseRaw(out string) ([]change, error) {
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if len(fields) == 1 && fields[0] == "" {
		return nil, nil
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("unexpected raw diff output")
	}
	var out2 []change
	for i := 0; i < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 {
			return nil, fmt.Errorf("unexpected raw diff entry %q", fields[i])
		}
		out2 = append(out2, change{OldMode: meta[0], NewMode: meta[1], Status: meta[4], Path: fields[i+1]})
	}
	return out2, nil
}

// checkChanges applies checkPath to every staged file, and allows only
// regular files whose mode does not change: no symlinks (a link could point a
// later read or render outside the tree), no submodules, no new executables.
func checkChanges(changes []change) error {
	if len(changes) == 0 {
		return reject("the patch changes nothing")
	}
	if len(changes) > maxFiles {
		return reject("the patch touches %d files; at most %d are allowed", len(changes), maxFiles)
	}
	for _, c := range changes {
		if err := checkPath(c.Path); err != nil {
			return err
		}
		switch c.Status {
		case "A":
			if c.NewMode != "100644" {
				return reject("%s: new files must be regular, non-executable files (mode %s)", c.Path, c.NewMode)
			}
		case "M":
			if c.OldMode != c.NewMode {
				return reject("%s: file modes may not change", c.Path)
			}
			if c.NewMode != "100644" && c.NewMode != "100755" {
				return reject("%s: only regular files may be changed (mode %s)", c.Path, c.NewMode)
			}
		case "D":
			if c.OldMode != "100644" && c.OldMode != "100755" {
				return reject("%s: only regular files may be deleted", c.Path)
			}
		default:
			return reject("%s: unsupported change type %s", c.Path, c.Status)
		}
	}
	return nil
}

// Limits on what the agent may hand over.
const (
	maxFiles    = 20
	maxPatch    = 256 << 10
	maxTitle    = 72
	maxBodyText = 20_000
)

// titleRE is a conventional-commit subject, the form every commit in the
// repository uses.
var titleRE = regexp.MustCompile(`^(fix|feat|chore|perf|refactor|revert)(\([a-z0-9._/-]+\))?: \S`)

// CheckTitle validates a PR title, which is also the commit subject and so
// is published on the public mirror. (The commit-msg hook then checks it for
// private values.)
func CheckTitle(title string) error {
	if strings.ContainsAny(title, "\r\n") {
		return reject("the title must be one line")
	}
	if len(title) > maxTitle {
		return reject("the title is %d characters; at most %d are allowed", len(title), maxTitle)
	}
	if !titleRE.MatchString(title) {
		return reject("the title must be a conventional commit subject, e.g. \"fix(ebooks): raise calibre memory limit\"")
	}
	return nil
}

// CheckBody bounds the PR description.
func CheckBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return reject("the body must explain the change")
	}
	if len(body) > maxBodyText {
		return reject("the body is %d characters; at most %d are allowed", len(body), maxBodyText)
	}
	return nil
}

// CheckPatch bounds the patch before anything runs on it.
func CheckPatch(patch string) error {
	if strings.TrimSpace(patch) == "" {
		return reject("the patch is empty; edit the files, then pass the output of git_diff")
	}
	if len(patch) > maxPatch {
		return reject("the patch is %d bytes; at most %d are allowed", len(patch), maxPatch)
	}
	if strings.Contains(patch, "GIT binary patch") {
		return reject("binary changes are not allowed")
	}
	return nil
}
