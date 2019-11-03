// Copyright 2015 The Gogs Authors. All rights reserved.
// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package models

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/process"
	"code.gitea.io/gitea/modules/setting"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/sync"
	"code.gitea.io/gitea/modules/timeutil"

	"github.com/unknwon/com"
)

var pullRequestQueue = sync.NewUniqueQueue(setting.Repository.PullRequestQueueLength)

// PullRequestType defines pull request type
type PullRequestType int

// Enumerate all the pull request types
const (
	PullRequestGitea PullRequestType = iota
	PullRequestGit
)

// PullRequestStatus defines pull request status
type PullRequestStatus int

// Enumerate all the pull request status
const (
	PullRequestStatusConflict PullRequestStatus = iota
	PullRequestStatusChecking
	PullRequestStatusMergeable
	PullRequestStatusManuallyMerged
)

// PullRequest represents relation between pull request and repositories.
type PullRequest struct {
	ID              int64 `xorm:"pk autoincr"`
	Type            PullRequestType
	Status          PullRequestStatus
	ConflictedFiles []string `xorm:"TEXT JSON"`

	IssueID int64  `xorm:"INDEX"`
	Issue   *Issue `xorm:"-"`
	Index   int64

	HeadRepoID      int64       `xorm:"INDEX"`
	HeadRepo        *Repository `xorm:"-"`
	BaseRepoID      int64       `xorm:"INDEX"`
	BaseRepo        *Repository `xorm:"-"`
	HeadBranch      string
	BaseBranch      string
	ProtectedBranch *ProtectedBranch `xorm:"-"`
	MergeBase       string           `xorm:"VARCHAR(40)"`

	HasMerged      bool               `xorm:"INDEX"`
	MergedCommitID string             `xorm:"VARCHAR(40)"`
	MergerID       int64              `xorm:"INDEX"`
	Merger         *User              `xorm:"-"`
	MergedUnix     timeutil.TimeStamp `xorm:"updated INDEX"`
}

// MustHeadUserName returns the HeadRepo's username if failed return blank
func (pr *PullRequest) MustHeadUserName() string {
	if err := pr.LoadHeadRepo(); err != nil {
		log.Error("LoadHeadRepo: %v", err)
		return ""
	}
	return pr.HeadRepo.MustOwnerName()
}

// Note: don't try to get Issue because will end up recursive querying.
func (pr *PullRequest) loadAttributes(e Engine) (err error) {
	if pr.HasMerged && pr.Merger == nil {
		pr.Merger, err = getUserByID(e, pr.MergerID)
		if IsErrUserNotExist(err) {
			pr.MergerID = -1
			pr.Merger = NewGhostUser()
		} else if err != nil {
			return fmt.Errorf("getUserByID [%d]: %v", pr.MergerID, err)
		}
	}

	return nil
}

// LoadAttributes loads pull request attributes from database
func (pr *PullRequest) LoadAttributes() error {
	return pr.loadAttributes(x)
}

// LoadBaseRepo loads pull request base repository from database
func (pr *PullRequest) LoadBaseRepo() error {
	if pr.BaseRepo == nil {
		if pr.HeadRepoID == pr.BaseRepoID && pr.HeadRepo != nil {
			pr.BaseRepo = pr.HeadRepo
			return nil
		}
		var repo Repository
		if has, err := x.ID(pr.BaseRepoID).Get(&repo); err != nil {
			return err
		} else if !has {
			return ErrRepoNotExist{ID: pr.BaseRepoID}
		}
		pr.BaseRepo = &repo
	}
	return nil
}

// LoadHeadRepo loads pull request head repository from database
func (pr *PullRequest) LoadHeadRepo() error {
	if pr.HeadRepo == nil {
		if pr.HeadRepoID == pr.BaseRepoID && pr.BaseRepo != nil {
			pr.HeadRepo = pr.BaseRepo
			return nil
		}
		var repo Repository
		if has, err := x.ID(pr.HeadRepoID).Get(&repo); err != nil {
			return err
		} else if !has {
			return ErrRepoNotExist{ID: pr.BaseRepoID}
		}
		pr.HeadRepo = &repo
	}
	return nil
}

// LoadIssue loads issue information from database
func (pr *PullRequest) LoadIssue() (err error) {
	return pr.loadIssue(x)
}

func (pr *PullRequest) loadIssue(e Engine) (err error) {
	if pr.Issue != nil {
		return nil
	}

	pr.Issue, err = getIssueByID(e, pr.IssueID)
	return err
}

// LoadProtectedBranch loads the protected branch of the base branch
func (pr *PullRequest) LoadProtectedBranch() (err error) {
	if pr.BaseRepo == nil {
		if pr.BaseRepoID == 0 {
			return nil
		}
		pr.BaseRepo, err = GetRepositoryByID(pr.BaseRepoID)
		if err != nil {
			return
		}
	}
	pr.ProtectedBranch, err = GetProtectedBranchBy(pr.BaseRepo.ID, pr.BaseBranch)
	return
}

// GetDefaultMergeMessage returns default message used when merging pull request
func (pr *PullRequest) GetDefaultMergeMessage() string {
	if pr.HeadRepo == nil {
		var err error
		pr.HeadRepo, err = GetRepositoryByID(pr.HeadRepoID)
		if err != nil {
			log.Error("GetRepositoryById[%d]: %v", pr.HeadRepoID, err)
			return ""
		}
	}
	return fmt.Sprintf("Merge branch '%s' of %s/%s into %s", pr.HeadBranch, pr.MustHeadUserName(), pr.HeadRepo.Name, pr.BaseBranch)
}

// GetDefaultSquashMessage returns default message used when squash and merging pull request
func (pr *PullRequest) GetDefaultSquashMessage() string {
	if err := pr.LoadIssue(); err != nil {
		log.Error("LoadIssue: %v", err)
		return ""
	}
	return fmt.Sprintf("%s (#%d)", pr.Issue.Title, pr.Issue.Index)
}

// GetGitRefName returns git ref for hidden pull request branch
func (pr *PullRequest) GetGitRefName() string {
	return fmt.Sprintf("refs/pull/%d/head", pr.Index)
}

// APIFormat assumes following fields have been assigned with valid values:
// Required - Issue
// Optional - Merger
func (pr *PullRequest) APIFormat() *api.PullRequest {
	return pr.apiFormat(x)
}

func (pr *PullRequest) apiFormat(e Engine) *api.PullRequest {
	var (
		baseBranch *git.Branch
		headBranch *git.Branch
		baseCommit *git.Commit
		headCommit *git.Commit
		err        error
	)
	if err = pr.Issue.loadRepo(e); err != nil {
		log.Error("loadRepo[%d]: %v", pr.ID, err)
		return nil
	}
	apiIssue := pr.Issue.apiFormat(e)
	if pr.BaseRepo == nil {
		pr.BaseRepo, err = getRepositoryByID(e, pr.BaseRepoID)
		if err != nil {
			log.Error("GetRepositoryById[%d]: %v", pr.ID, err)
			return nil
		}
	}
	if pr.HeadRepo == nil {
		pr.HeadRepo, err = getRepositoryByID(e, pr.HeadRepoID)
		if err != nil {
			log.Error("GetRepositoryById[%d]: %v", pr.ID, err)
			return nil
		}
	}

	if err = pr.Issue.loadRepo(e); err != nil {
		log.Error("pr.Issue.loadRepo[%d]: %v", pr.ID, err)
		return nil
	}

	apiPullRequest := &api.PullRequest{
		ID:        pr.ID,
		URL:       pr.Issue.HTMLURL(),
		Index:     pr.Index,
		Poster:    apiIssue.Poster,
		Title:     apiIssue.Title,
		Body:      apiIssue.Body,
		Labels:    apiIssue.Labels,
		Milestone: apiIssue.Milestone,
		Assignee:  apiIssue.Assignee,
		Assignees: apiIssue.Assignees,
		State:     apiIssue.State,
		Comments:  apiIssue.Comments,
		HTMLURL:   pr.Issue.HTMLURL(),
		DiffURL:   pr.Issue.DiffURL(),
		PatchURL:  pr.Issue.PatchURL(),
		HasMerged: pr.HasMerged,
		MergeBase: pr.MergeBase,
		Deadline:  apiIssue.Deadline,
		Created:   pr.Issue.CreatedUnix.AsTimePtr(),
		Updated:   pr.Issue.UpdatedUnix.AsTimePtr(),
	}
	baseBranch, err = pr.BaseRepo.GetBranch(pr.BaseBranch)
	if err != nil {
		if git.IsErrBranchNotExist(err) {
			apiPullRequest.Base = nil
		} else {
			log.Error("GetBranch[%s]: %v", pr.BaseBranch, err)
			return nil
		}
	} else {
		apiBaseBranchInfo := &api.PRBranchInfo{
			Name:       pr.BaseBranch,
			Ref:        pr.BaseBranch,
			RepoID:     pr.BaseRepoID,
			Repository: pr.BaseRepo.innerAPIFormat(e, AccessModeNone, false),
		}
		baseCommit, err = baseBranch.GetCommit()
		if err != nil {
			if git.IsErrNotExist(err) {
				apiBaseBranchInfo.Sha = ""
			} else {
				log.Error("GetCommit[%s]: %v", baseBranch.Name, err)
				return nil
			}
		} else {
			apiBaseBranchInfo.Sha = baseCommit.ID.String()
		}
		apiPullRequest.Base = apiBaseBranchInfo
	}

	headBranch, err = pr.HeadRepo.GetBranch(pr.HeadBranch)
	if err != nil {
		if git.IsErrBranchNotExist(err) {
			apiPullRequest.Head = nil
		} else {
			log.Error("GetBranch[%s]: %v", pr.HeadBranch, err)
			return nil
		}
	} else {
		apiHeadBranchInfo := &api.PRBranchInfo{
			Name:       pr.HeadBranch,
			Ref:        pr.HeadBranch,
			RepoID:     pr.HeadRepoID,
			Repository: pr.HeadRepo.innerAPIFormat(e, AccessModeNone, false),
		}
		headCommit, err = headBranch.GetCommit()
		if err != nil {
			if git.IsErrNotExist(err) {
				apiHeadBranchInfo.Sha = ""
			} else {
				log.Error("GetCommit[%s]: %v", headBranch.Name, err)
				return nil
			}
		} else {
			apiHeadBranchInfo.Sha = headCommit.ID.String()
		}
		apiPullRequest.Head = apiHeadBranchInfo
	}

	if pr.Status != PullRequestStatusChecking {
		mergeable := pr.Status != PullRequestStatusConflict && !pr.IsWorkInProgress()
		apiPullRequest.Mergeable = mergeable
	}
	if pr.HasMerged {
		apiPullRequest.Merged = pr.MergedUnix.AsTimePtr()
		apiPullRequest.MergedCommitID = &pr.MergedCommitID
		apiPullRequest.MergedBy = pr.Merger.APIFormat()
	}

	return apiPullRequest
}

func (pr *PullRequest) getHeadRepo(e Engine) (err error) {
	pr.HeadRepo, err = getRepositoryByID(e, pr.HeadRepoID)
	if err != nil && !IsErrRepoNotExist(err) {
		return fmt.Errorf("getRepositoryByID(head): %v", err)
	}
	return nil
}

// GetHeadRepo loads the head repository
func (pr *PullRequest) GetHeadRepo() error {
	return pr.getHeadRepo(x)
}

// GetBaseRepo loads the target repository
func (pr *PullRequest) GetBaseRepo() (err error) {
	if pr.BaseRepo != nil {
		return nil
	}

	pr.BaseRepo, err = GetRepositoryByID(pr.BaseRepoID)
	if err != nil {
		return fmt.Errorf("GetRepositoryByID(base): %v", err)
	}
	return nil
}

// IsChecking returns true if this pull request is still checking conflict.
func (pr *PullRequest) IsChecking() bool {
	return pr.Status == PullRequestStatusChecking
}

// CanAutoMerge returns true if this pull request can be merged automatically.
func (pr *PullRequest) CanAutoMerge() bool {
	return pr.Status == PullRequestStatusMergeable
}

// GetLastCommitStatus returns the last commit status for this pull request.
func (pr *PullRequest) GetLastCommitStatus() (status *CommitStatus, err error) {
	if err = pr.GetHeadRepo(); err != nil {
		return nil, err
	}

	if pr.HeadRepo == nil {
		return nil, ErrPullRequestHeadRepoMissing{pr.ID, pr.HeadRepoID}
	}

	headGitRepo, err := git.OpenRepository(pr.HeadRepo.RepoPath())
	if err != nil {
		return nil, err
	}

	lastCommitID, err := headGitRepo.GetBranchCommitID(pr.HeadBranch)
	if err != nil {
		return nil, err
	}

	err = pr.LoadBaseRepo()
	if err != nil {
		return nil, err
	}

	statusList, err := GetLatestCommitStatus(pr.BaseRepo, lastCommitID, 0)
	if err != nil {
		return nil, err
	}
	return CalcCommitStatus(statusList), nil
}

// MergeStyle represents the approach to merge commits into base branch.
type MergeStyle string

const (
	// MergeStyleMerge create merge commit
	MergeStyleMerge MergeStyle = "merge"
	// MergeStyleRebase rebase before merging
	MergeStyleRebase MergeStyle = "rebase"
	// MergeStyleRebaseMerge rebase before merging with merge commit (--no-ff)
	MergeStyleRebaseMerge MergeStyle = "rebase-merge"
	// MergeStyleSquash squash commits into single commit before merging
	MergeStyleSquash MergeStyle = "squash"
)

// CheckUserAllowedToMerge checks whether the user is allowed to merge
func (pr *PullRequest) CheckUserAllowedToMerge(doer *User) (err error) {
	if doer == nil {
		return ErrNotAllowedToMerge{
			"Not signed in",
		}
	}

	if pr.BaseRepo == nil {
		if err = pr.GetBaseRepo(); err != nil {
			return fmt.Errorf("GetBaseRepo: %v", err)
		}
	}

	if protected, err := pr.BaseRepo.IsProtectedBranchForMerging(pr, pr.BaseBranch, doer); err != nil {
		return fmt.Errorf("IsProtectedBranch: %v", err)
	} else if protected {
		return ErrNotAllowedToMerge{
			"The branch is protected",
		}
	}

	return nil
}

// SetMerged sets a pull request to merged and closes the corresponding issue
func (pr *PullRequest) SetMerged() (err error) {
	if pr.HasMerged {
		return fmt.Errorf("PullRequest[%d] already merged", pr.Index)
	}
	if pr.MergedCommitID == "" || pr.MergedUnix == 0 || pr.Merger == nil {
		return fmt.Errorf("Unable to merge PullRequest[%d], some required fields are empty", pr.Index)
	}

	pr.HasMerged = true

	sess := x.NewSession()
	defer sess.Close()
	if err = sess.Begin(); err != nil {
		return err
	}

	if err = pr.loadIssue(sess); err != nil {
		return err
	}

	if err = pr.Issue.loadRepo(sess); err != nil {
		return err
	}
	if err = pr.Issue.Repo.getOwner(sess); err != nil {
		return err
	}

	if err = pr.Issue.changeStatus(sess, pr.Merger, true); err != nil {
		return fmt.Errorf("Issue.changeStatus: %v", err)
	}
	if _, err = sess.ID(pr.ID).Cols("has_merged, status, merged_commit_id, merger_id, merged_unix").Update(pr); err != nil {
		return fmt.Errorf("update pull request: %v", err)
	}

	if err = sess.Commit(); err != nil {
		return fmt.Errorf("Commit: %v", err)
	}
	return nil
}

// manuallyMerged checks if a pull request got manually merged
// When a pull request got manually merged mark the pull request as merged
func (pr *PullRequest) manuallyMerged() bool {
	commit, err := pr.getMergeCommit()
	if err != nil {
		log.Error("PullRequest[%d].getMergeCommit: %v", pr.ID, err)
		return false
	}
	if commit != nil {
		pr.MergedCommitID = commit.ID.String()
		pr.MergedUnix = timeutil.TimeStamp(commit.Author.When.Unix())
		pr.Status = PullRequestStatusManuallyMerged
		merger, _ := GetUserByEmail(commit.Author.Email)

		// When the commit author is unknown set the BaseRepo owner as merger
		if merger == nil {
			if pr.BaseRepo.Owner == nil {
				if err = pr.BaseRepo.getOwner(x); err != nil {
					log.Error("BaseRepo.getOwner[%d]: %v", pr.ID, err)
					return false
				}
			}
			merger = pr.BaseRepo.Owner
		}
		pr.Merger = merger
		pr.MergerID = merger.ID

		if err = pr.SetMerged(); err != nil {
			log.Error("PullRequest[%d].setMerged : %v", pr.ID, err)
			return false
		}
		log.Info("manuallyMerged[%d]: Marked as manually merged into %s/%s by commit id: %s", pr.ID, pr.BaseRepo.Name, pr.BaseBranch, commit.ID.String())
		return true
	}
	return false
}

// getMergeCommit checks if a pull request got merged
// Returns the git.Commit of the pull request if merged
func (pr *PullRequest) getMergeCommit() (*git.Commit, error) {
	if pr.BaseRepo == nil {
		var err error
		pr.BaseRepo, err = GetRepositoryByID(pr.BaseRepoID)
		if err != nil {
			return nil, fmt.Errorf("GetRepositoryByID: %v", err)
		}
	}

	indexTmpPath := filepath.Join(os.TempDir(), "gitea-"+pr.BaseRepo.Name+"-"+strconv.Itoa(time.Now().Nanosecond()))
	defer os.Remove(indexTmpPath)

	headFile := pr.GetGitRefName()

	// Check if a pull request is merged into BaseBranch
	_, stderr, err := process.GetManager().ExecDirEnv(-1, "", fmt.Sprintf("isMerged (git merge-base --is-ancestor): %d", pr.BaseRepo.ID),
		[]string{"GIT_INDEX_FILE=" + indexTmpPath, "GIT_DIR=" + pr.BaseRepo.RepoPath()},
		git.GitExecutable, "merge-base", "--is-ancestor", headFile, pr.BaseBranch)

	if err != nil {
		// Errors are signaled by a non-zero status that is not 1
		if strings.Contains(err.Error(), "exit status 1") {
			return nil, nil
		}
		return nil, fmt.Errorf("git merge-base --is-ancestor: %v %v", stderr, err)
	}

	commitIDBytes, err := ioutil.ReadFile(pr.BaseRepo.RepoPath() + "/" + headFile)
	if err != nil {
		return nil, fmt.Errorf("ReadFile(%s): %v", headFile, err)
	}
	commitID := string(commitIDBytes)
	if len(commitID) < 40 {
		return nil, fmt.Errorf(`ReadFile(%s): invalid commit-ID "%s"`, headFile, commitID)
	}
	cmd := commitID[:40] + ".." + pr.BaseBranch

	// Get the commit from BaseBranch where the pull request got merged
	mergeCommit, stderr, err := process.GetManager().ExecDirEnv(-1, "", fmt.Sprintf("isMerged (git rev-list --ancestry-path --merges --reverse): %d", pr.BaseRepo.ID),
		[]string{"GIT_INDEX_FILE=" + indexTmpPath, "GIT_DIR=" + pr.BaseRepo.RepoPath()},
		git.GitExecutable, "rev-list", "--ancestry-path", "--merges", "--reverse", cmd)
	if err != nil {
		return nil, fmt.Errorf("git rev-list --ancestry-path --merges --reverse: %v %v", stderr, err)
	} else if len(mergeCommit) < 40 {
		// PR was fast-forwarded, so just use last commit of PR
		mergeCommit = commitID[:40]
	}

	gitRepo, err := git.OpenRepository(pr.BaseRepo.RepoPath())
	if err != nil {
		return nil, fmt.Errorf("OpenRepository: %v", err)
	}

	commit, err := gitRepo.GetCommit(mergeCommit[:40])
	if err != nil {
		return nil, fmt.Errorf("GetCommit: %v", err)
	}

	return commit, nil
}

// patchConflicts is a list of conflict description from Git.
var patchConflicts = []string{
	"patch does not apply",
	"already exists in working directory",
	"unrecognized input",
	"error:",
}

// testPatch checks if patch can be merged to base repository without conflict.
func (pr *PullRequest) testPatch(e Engine) (err error) {
	if pr.BaseRepo == nil {
		pr.BaseRepo, err = getRepositoryByID(e, pr.BaseRepoID)
		if err != nil {
			return fmt.Errorf("GetRepositoryByID: %v", err)
		}
	}

	patchPath, err := pr.BaseRepo.patchPath(e, pr.Index)
	if err != nil {
		return fmt.Errorf("BaseRepo.PatchPath: %v", err)
	}

	// Fast fail if patch does not exist, this assumes data is corrupted.
	if !com.IsFile(patchPath) {
		log.Trace("PullRequest[%d].testPatch: ignored corrupted data", pr.ID)
		return nil
	}

	repoWorkingPool.CheckIn(com.ToStr(pr.BaseRepoID))
	defer repoWorkingPool.CheckOut(com.ToStr(pr.BaseRepoID))

	log.Trace("PullRequest[%d].testPatch (patchPath): %s", pr.ID, patchPath)

	pr.Status = PullRequestStatusChecking

	indexTmpPath := filepath.Join(os.TempDir(), "gitea-"+pr.BaseRepo.Name+"-"+strconv.Itoa(time.Now().Nanosecond()))
	defer os.Remove(indexTmpPath)

	var stderr string
	_, stderr, err = process.GetManager().ExecDirEnv(-1, "", fmt.Sprintf("testPatch (git read-tree): %d", pr.BaseRepo.ID),
		[]string{"GIT_DIR=" + pr.BaseRepo.RepoPath(), "GIT_INDEX_FILE=" + indexTmpPath},
		git.GitExecutable, "read-tree", pr.BaseBranch)
	if err != nil {
		return fmt.Errorf("git read-tree --index-output=%s %s: %v - %s", indexTmpPath, pr.BaseBranch, err, stderr)
	}

	prUnit, err := pr.BaseRepo.getUnit(e, UnitTypePullRequests)
	if err != nil {
		return err
	}
	prConfig := prUnit.PullRequestsConfig()

	args := []string{"apply", "--check", "--cached"}
	if prConfig.IgnoreWhitespaceConflicts {
		args = append(args, "--ignore-whitespace")
	}
	args = append(args, patchPath)
	pr.ConflictedFiles = []string{}

	_, stderr, err = process.GetManager().ExecDirEnv(-1, "", fmt.Sprintf("testPatch (git apply --check): %d", pr.BaseRepo.ID),
		[]string{"GIT_INDEX_FILE=" + indexTmpPath, "GIT_DIR=" + pr.BaseRepo.RepoPath()},
		git.GitExecutable, args...)
	if err != nil {
		for i := range patchConflicts {
			if strings.Contains(stderr, patchConflicts[i]) {
				log.Trace("PullRequest[%d].testPatch (apply): has conflict: %s", pr.ID, stderr)
				const prefix = "error: patch failed:"
				pr.Status = PullRequestStatusConflict
				pr.ConflictedFiles = make([]string, 0, 5)
				scanner := bufio.NewScanner(strings.NewReader(stderr))
				for scanner.Scan() {
					line := scanner.Text()

					if strings.HasPrefix(line, prefix) {
						var found bool
						var filepath = strings.TrimSpace(strings.Split(line[len(prefix):], ":")[0])
						for _, f := range pr.ConflictedFiles {
							if f == filepath {
								found = true
								break
							}
						}
						if !found {
							pr.ConflictedFiles = append(pr.ConflictedFiles, filepath)
						}
					}
					// only list 10 conflicted files
					if len(pr.ConflictedFiles) >= 10 {
						break
					}
				}

				if len(pr.ConflictedFiles) > 0 {
					log.Trace("Found %d files conflicted: %v", len(pr.ConflictedFiles), pr.ConflictedFiles)
				}

				return nil
			}
		}

		return fmt.Errorf("git apply --check: %v - %s", err, stderr)
	}
	return nil
}

// NewPullRequest creates new pull request with labels for repository.
func NewPullRequest(repo *Repository, pull *Issue, labelIDs []int64, uuids []string, pr *PullRequest, patch []byte) (err error) {
	// Retry several times in case INSERT fails due to duplicate key for (repo_id, index); see #7887
	i := 0
	for {
		if err = newPullRequestAttempt(repo, pull, labelIDs, uuids, pr, patch); err == nil {
			return nil
		}
		if !IsErrNewIssueInsert(err) {
			return err
		}
		if i++; i == issueMaxDupIndexAttempts {
			break
		}
		log.Error("NewPullRequest: error attempting to insert the new issue; will retry. Original error: %v", err)
	}
	return fmt.Errorf("NewPullRequest: too many errors attempting to insert the new issue. Last error was: %v", err)
}

func newPullRequestAttempt(repo *Repository, pull *Issue, labelIDs []int64, uuids []string, pr *PullRequest, patch []byte) (err error) {
	sess := x.NewSession()
	defer sess.Close()
	if err = sess.Begin(); err != nil {
		return err
	}

	if err = newIssue(sess, pull.Poster, NewIssueOptions{
		Repo:        repo,
		Issue:       pull,
		LabelIDs:    labelIDs,
		Attachments: uuids,
		IsPull:      true,
	}); err != nil {
		if IsErrUserDoesNotHaveAccessToRepo(err) || IsErrNewIssueInsert(err) {
			return err
		}
		return fmt.Errorf("newIssue: %v", err)
	}

	pr.Index = pull.Index
	pr.BaseRepo = repo
	pr.Status = PullRequestStatusChecking
	if len(patch) > 0 {
		if err = repo.savePatch(sess, pr.Index, patch); err != nil {
			return fmt.Errorf("SavePatch: %v", err)
		}

		if err = pr.testPatch(sess); err != nil {
			return fmt.Errorf("testPatch: %v", err)
		}
	}
	// No conflict appears after test means mergeable.
	if pr.Status == PullRequestStatusChecking {
		pr.Status = PullRequestStatusMergeable
	}

	pr.IssueID = pull.ID
	if _, err = sess.Insert(pr); err != nil {
		return fmt.Errorf("insert pull repo: %v", err)
	}

	if err = sess.Commit(); err != nil {
		return fmt.Errorf("Commit: %v", err)
	}

	return nil
}

// GetUnmergedPullRequest returns a pull request that is open and has not been merged
// by given head/base and repo/branch.
func GetUnmergedPullRequest(headRepoID, baseRepoID int64, headBranch, baseBranch string) (*PullRequest, error) {
	pr := new(PullRequest)
	has, err := x.
		Where("head_repo_id=? AND head_branch=? AND base_repo_id=? AND base_branch=? AND has_merged=? AND issue.is_closed=?",
			headRepoID, headBranch, baseRepoID, baseBranch, false, false).
		Join("INNER", "issue", "issue.id=pull_request.issue_id").
		Get(pr)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrPullRequestNotExist{0, 0, headRepoID, baseRepoID, headBranch, baseBranch}
	}

	return pr, nil
}

// GetLatestPullRequestByHeadInfo returns the latest pull request (regardless of its status)
// by given head information (repo and branch).
func GetLatestPullRequestByHeadInfo(repoID int64, branch string) (*PullRequest, error) {
	pr := new(PullRequest)
	has, err := x.
		Where("head_repo_id = ? AND head_branch = ?", repoID, branch).
		OrderBy("id DESC").
		Get(pr)
	if !has {
		return nil, err
	}
	return pr, err
}

// GetPullRequestByIndex returns a pull request by the given index
func GetPullRequestByIndex(repoID int64, index int64) (*PullRequest, error) {
	pr := &PullRequest{
		BaseRepoID: repoID,
		Index:      index,
	}

	has, err := x.Get(pr)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrPullRequestNotExist{0, 0, 0, repoID, "", ""}
	}

	if err = pr.LoadAttributes(); err != nil {
		return nil, err
	}
	if err = pr.LoadIssue(); err != nil {
		return nil, err
	}

	return pr, nil
}

func getPullRequestByID(e Engine, id int64) (*PullRequest, error) {
	pr := new(PullRequest)
	has, err := e.ID(id).Get(pr)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrPullRequestNotExist{id, 0, 0, 0, "", ""}
	}
	return pr, pr.loadAttributes(e)
}

// GetPullRequestByID returns a pull request by given ID.
func GetPullRequestByID(id int64) (*PullRequest, error) {
	return getPullRequestByID(x, id)
}

func getPullRequestByIssueID(e Engine, issueID int64) (*PullRequest, error) {
	pr := &PullRequest{
		IssueID: issueID,
	}
	has, err := e.Get(pr)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrPullRequestNotExist{0, issueID, 0, 0, "", ""}
	}
	return pr, pr.loadAttributes(e)
}

// GetPullRequestByIssueID returns pull request by given issue ID.
func GetPullRequestByIssueID(issueID int64) (*PullRequest, error) {
	return getPullRequestByIssueID(x, issueID)
}

// Update updates all fields of pull request.
func (pr *PullRequest) Update() error {
	_, err := x.ID(pr.ID).AllCols().Update(pr)
	return err
}

// UpdateCols updates specific fields of pull request.
func (pr *PullRequest) UpdateCols(cols ...string) error {
	_, err := x.ID(pr.ID).Cols(cols...).Update(pr)
	return err
}

// UpdatePatch generates and saves a new patch.
func (pr *PullRequest) UpdatePatch() (err error) {
	if err = pr.GetHeadRepo(); err != nil {
		return fmt.Errorf("GetHeadRepo: %v", err)
	} else if pr.HeadRepo == nil {
		log.Trace("PullRequest[%d].UpdatePatch: ignored corrupted data", pr.ID)
		return nil
	}

	if err = pr.GetBaseRepo(); err != nil {
		return fmt.Errorf("GetBaseRepo: %v", err)
	}

	headGitRepo, err := git.OpenRepository(pr.HeadRepo.RepoPath())
	if err != nil {
		return fmt.Errorf("OpenRepository: %v", err)
	}

	// Add a temporary remote.
	tmpRemote := com.ToStr(time.Now().UnixNano())
	if err = headGitRepo.AddRemote(tmpRemote, RepoPath(pr.BaseRepo.MustOwner().Name, pr.BaseRepo.Name), true); err != nil {
		return fmt.Errorf("AddRemote: %v", err)
	}
	defer func() {
		if err := headGitRepo.RemoveRemote(tmpRemote); err != nil {
			log.Error("UpdatePatch: RemoveRemote: %s", err)
		}
	}()
	pr.MergeBase, _, err = headGitRepo.GetMergeBase(tmpRemote, pr.BaseBranch, pr.HeadBranch)
	if err != nil {
		return fmt.Errorf("GetMergeBase: %v", err)
	} else if err = pr.Update(); err != nil {
		return fmt.Errorf("Update: %v", err)
	}

	patch, err := headGitRepo.GetPatch(pr.MergeBase, pr.HeadBranch)
	if err != nil {
		return fmt.Errorf("GetPatch: %v", err)
	}

	if err = pr.BaseRepo.SavePatch(pr.Index, patch); err != nil {
		return fmt.Errorf("BaseRepo.SavePatch: %v", err)
	}

	return nil
}

// PushToBaseRepo pushes commits from branches of head repository to
// corresponding branches of base repository.
// FIXME: Only push branches that are actually updates?
func (pr *PullRequest) PushToBaseRepo() (err error) {
	log.Trace("PushToBaseRepo[%d]: pushing commits to base repo '%s'", pr.BaseRepoID, pr.GetGitRefName())

	headRepoPath := pr.HeadRepo.RepoPath()
	headGitRepo, err := git.OpenRepository(headRepoPath)
	if err != nil {
		return fmt.Errorf("OpenRepository: %v", err)
	}

	tmpRemoteName := fmt.Sprintf("tmp-pull-%d", pr.ID)
	if err = headGitRepo.AddRemote(tmpRemoteName, pr.BaseRepo.RepoPath(), false); err != nil {
		return fmt.Errorf("headGitRepo.AddRemote: %v", err)
	}
	// Make sure to remove the remote even if the push fails
	defer func() {
		if err := headGitRepo.RemoveRemote(tmpRemoteName); err != nil {
			log.Error("PushToBaseRepo: RemoveRemote: %s", err)
		}
	}()

	headFile := pr.GetGitRefName()

	// Remove head in case there is a conflict.
	file := path.Join(pr.BaseRepo.RepoPath(), headFile)

	_ = os.Remove(file)

	if err = git.Push(headRepoPath, git.PushOptions{
		Remote: tmpRemoteName,
		Branch: fmt.Sprintf("%s:%s", pr.HeadBranch, headFile),
		Force:  true,
	}); err != nil {
		return fmt.Errorf("Push: %v", err)
	}

	return nil
}

// AddToTaskQueue adds itself to pull request test task queue.
func (pr *PullRequest) AddToTaskQueue() {
	go pullRequestQueue.AddFunc(pr.ID, func() {
		pr.Status = PullRequestStatusChecking
		if err := pr.UpdateCols("status"); err != nil {
			log.Error("AddToTaskQueue.UpdateCols[%d].(add to queue): %v", pr.ID, err)
		}
	})
}

// checkAndUpdateStatus checks if pull request is possible to leaving checking status,
// and set to be either conflict or mergeable.
func (pr *PullRequest) checkAndUpdateStatus() {
	// Status is not changed to conflict means mergeable.
	if pr.Status == PullRequestStatusChecking {
		pr.Status = PullRequestStatusMergeable
	}

	// Make sure there is no waiting test to process before leaving the checking status.
	if !pullRequestQueue.Exist(pr.ID) {
		if err := pr.UpdateCols("status, conflicted_files"); err != nil {
			log.Error("Update[%d]: %v", pr.ID, err)
		}
	}
}

// IsWorkInProgress determine if the Pull Request is a Work In Progress by its title
func (pr *PullRequest) IsWorkInProgress() bool {
	if err := pr.LoadIssue(); err != nil {
		log.Error("LoadIssue: %v", err)
		return false
	}

	for _, prefix := range setting.Repository.PullRequest.WorkInProgressPrefixes {
		if strings.HasPrefix(strings.ToUpper(pr.Issue.Title), prefix) {
			return true
		}
	}
	return false
}

// IsFilesConflicted determines if the  Pull Request has changes conflicting with the target branch.
func (pr *PullRequest) IsFilesConflicted() bool {
	return len(pr.ConflictedFiles) > 0
}

// GetWorkInProgressPrefix returns the prefix used to mark the pull request as a work in progress.
// It returns an empty string when none were found
func (pr *PullRequest) GetWorkInProgressPrefix() string {
	if err := pr.LoadIssue(); err != nil {
		log.Error("LoadIssue: %v", err)
		return ""
	}

	for _, prefix := range setting.Repository.PullRequest.WorkInProgressPrefixes {
		if strings.HasPrefix(strings.ToUpper(pr.Issue.Title), prefix) {
			return pr.Issue.Title[0:len(prefix)]
		}
	}
	return ""
}
