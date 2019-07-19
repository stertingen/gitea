// Copyright 2014 The Gogs Authors. All rights reserved.
// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package cron

import (
	"time"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/sync"

	"github.com/gogs/cron"
)

const (
	mirrorUpdate           = "mirror_update"
	gitFsck                = "git_fsck"
	checkRepos             = "check_repos"
	archiveCleanup         = "archive_cleanup"
	syncExternalUsers      = "sync_external_users"
	deletedBranchesCleanup = "deleted_branches_cleanup"
)

var c = cron.New()

// Prevent duplicate running tasks.
var taskStatusTable = sync.NewStatusTable()

// Func defines a cron function body
type Func func()

// WithUnique wrap a cron func with an unique running check
func WithUnique(name string, body Func) Func {
	return func() {
		if !taskStatusTable.StartIfNotRunning(name) {
			return
		}
		defer taskStatusTable.Stop(name)
		body()
	}
}

// NewContext begins cron tasks
func NewContext() {
	var (
		entry *cron.Entry
		err   error
	)
	if setting.Cron.UpdateMirror.Enabled {
		entry, err = c.AddFunc("Update mirrors", setting.Cron.UpdateMirror.Schedule, WithUnique(mirrorUpdate, models.MirrorUpdate))
		if err != nil {
			log.Fatal("Cron[Update mirrors]: %v", err)
		}
		if setting.Cron.UpdateMirror.RunAtStart {
			entry.Prev = time.Now()
			entry.ExecTimes++
			go WithUnique(mirrorUpdate, models.MirrorUpdate)()
		}
	}
	if setting.Cron.RepoHealthCheck.Enabled {
		entry, err = c.AddFunc("Repository health check", setting.Cron.RepoHealthCheck.Schedule, WithUnique(gitFsck, models.GitFsck))
		if err != nil {
			log.Fatal("Cron[Repository health check]: %v", err)
		}
		if setting.Cron.RepoHealthCheck.RunAtStart {
			entry.Prev = time.Now()
			entry.ExecTimes++
			go WithUnique(gitFsck, models.GitFsck)()
		}
	}
	if setting.Cron.CheckRepoStats.Enabled {
		entry, err = c.AddFunc("Check repository statistics", setting.Cron.CheckRepoStats.Schedule, WithUnique(checkRepos, models.CheckRepoStats))
		if err != nil {
			log.Fatal("Cron[Check repository statistics]: %v", err)
		}
		if setting.Cron.CheckRepoStats.RunAtStart {
			entry.Prev = time.Now()
			entry.ExecTimes++
			go WithUnique(checkRepos, models.CheckRepoStats)()
		}
	}
	if setting.Cron.ArchiveCleanup.Enabled {
		entry, err = c.AddFunc("Clean up old repository archives", setting.Cron.ArchiveCleanup.Schedule, WithUnique(archiveCleanup, models.DeleteOldRepositoryArchives))
		if err != nil {
			log.Fatal("Cron[Clean up old repository archives]: %v", err)
		}
		if setting.Cron.ArchiveCleanup.RunAtStart {
			entry.Prev = time.Now()
			entry.ExecTimes++
			go WithUnique(archiveCleanup, models.DeleteOldRepositoryArchives)()
		}
	}
	if setting.Cron.SyncExternalUsers.Enabled {
		entry, err = c.AddFunc("Synchronize external users", setting.Cron.SyncExternalUsers.Schedule, WithUnique(syncExternalUsers, models.SyncExternalUsers))
		if err != nil {
			log.Fatal("Cron[Synchronize external users]: %v", err)
		}
		if setting.Cron.SyncExternalUsers.RunAtStart {
			entry.Prev = time.Now()
			entry.ExecTimes++
			go WithUnique(syncExternalUsers, models.SyncExternalUsers)()
		}
	}
	if setting.Cron.DeletedBranchesCleanup.Enabled {
		entry, err = c.AddFunc("Remove old deleted branches", setting.Cron.DeletedBranchesCleanup.Schedule, WithUnique(deletedBranchesCleanup, models.RemoveOldDeletedBranches))
		if err != nil {
			log.Fatal("Cron[Remove old deleted branches]: %v", err)
		}
		if setting.Cron.DeletedBranchesCleanup.RunAtStart {
			entry.Prev = time.Now()
			entry.ExecTimes++
			go WithUnique(deletedBranchesCleanup, models.RemoveOldDeletedBranches)()
		}
	}
	c.Start()
}

// ListTasks returns all running cron tasks.
func ListTasks() []*cron.Entry {
	return c.Entries()
}
