// Copyright 2025 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	actions_model "code.gitea.io/gitea/models/actions"
	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActionsJobTokenAccess(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		t.Run("Write Access", testActionsJobTokenAccess(u, false))
		t.Run("Read Access", testActionsJobTokenAccess(u, true))
	})
}

func testActionsJobTokenAccess(u *url.URL, isFork bool) func(t *testing.T) {
	return func(t *testing.T) {
		task := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: 47})
		require.NoError(t, task.GenerateToken())
		task.Status = actions_model.StatusRunning
		task.IsForkPullRequest = isFork
		err := actions_model.UpdateTask(t.Context(), task, "token_hash", "token_salt", "token_last_eight", "status", "is_fork_pull_request")
		task.LoadJob(t.Context())
		require.NoError(t, err)
		job := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: 192})
		job.WorkflowPayload = []byte("name: Push\n\"on\": push\njobs:\n    wf2-job:\n        name: wf2-job\n        runs-on: ubuntu-latest\n        steps:\n            - run: echo 'cmd 1'\n            - run: echo 'cmd 2'\n        permissions:\n            contents: write\n")
		_, err = actions_model.UpdateRunJob(t.Context(), job, nil, "workflow_payload")
		require.NoError(t, err)
		session := emptyTestSession(t)
		context := APITestContext{
			Session:  session,
			Token:    task.Token,
			Username: "user5",
			Reponame: "repo4",
		}
		dstPath := t.TempDir()

		u.Path = context.GitPath()
		u.User = url.UserPassword("gitea-actions", task.Token)

		t.Run("Git Clone", doGitClone(dstPath, u))

		t.Run("API Get Repository", doAPIGetRepository(context, func(t *testing.T, r structs.Repository) {
			require.Equal(t, "repo4", r.Name)
			require.Equal(t, "user5", r.Owner.UserName)
		}))

		context.ExpectedCode = util.Iif(isFork, http.StatusForbidden, http.StatusCreated)
		t.Run("API Create File", doAPICreateFile(context, "test.txt", &structs.CreateFileOptions{
			FileOptions: structs.FileOptions{
				NewBranchName: "new-branch",
				Message:       "Create File",
			},
			ContentBase64: base64.StdEncoding.EncodeToString([]byte(`This is a test file created using job token.`)),
		}))

		context.ExpectedCode = http.StatusForbidden
		t.Run("Fail to Create Repository", doAPICreateRepository(context, true))

		context.ExpectedCode = http.StatusForbidden
		t.Run("Fail to Delete Repository", doAPIDeleteRepository(context))

		t.Run("Fail to Create Organization", doAPICreateOrganization(context, &structs.CreateOrgOption{
			UserName: "actions",
			FullName: "Gitea Actions",
		}))
	}
}

func TestActionsJobTokenAccessLFS(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		httpContext := NewAPITestContext(t, "user2", "repo-lfs-test", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteRepository)
		t.Run("Create Repository", doAPICreateRepository(httpContext, false, func(t *testing.T, repository structs.Repository) {
			task := &actions_model.ActionTask{}
			require.NoError(t, task.GenerateToken())
			task.Status = actions_model.StatusRunning
			task.IsForkPullRequest = false
			task.RepoID = repository.ID
			err := db.Insert(t.Context(), task)
			require.NoError(t, err)
			session := emptyTestSession(t)
			httpContext := APITestContext{
				Session:  session,
				Token:    task.Token,
				Username: "user2",
				Reponame: "repo-lfs-test",
			}

			u.Path = httpContext.GitPath()
			dstPath := t.TempDir()

			u.Path = httpContext.GitPath()
			u.User = url.UserPassword("gitea-actions", task.Token)

			t.Run("Clone", doGitClone(dstPath, u))

			dstPath2 := t.TempDir()

			t.Run("Partial Clone", doPartialGitClone(dstPath2, u))

			lfs := lfsCommitAndPushTest(t, dstPath, testFileSizeSmall)[0]

			reqLFS := NewRequest(t, "GET", "/api/v1/repos/user2/repo-lfs-test/media/"+lfs).AddTokenAuth(task.Token)
			respLFS := MakeRequestNilResponseRecorder(t, reqLFS, http.StatusOK)
			assert.Equal(t, testFileSizeSmall, respLFS.Length)
		}))
	})
}

func TestActionsJobTokenPackageAccess(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	publicOrgNoMember := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 17})

	job := &actions_model.ActionRunJob{}
	job.WorkflowPayload = []byte("name: Push\n\"on\": push\njobs:\n    wf2-job:\n        name: wf2-job\n        runs-on: ubuntu-latest\n        steps:\n            - run: echo 'cmd 1'\n            - run: echo 'cmd 2'\n        permissions:\n            packages: write\n")
	err := db.Insert(t.Context(), job)
	require.NoError(t, err)

	task := &actions_model.ActionTask{}
	require.NoError(t, task.GenerateToken())
	task.Status = actions_model.StatusRunning
	task.IsForkPullRequest = false
	task.JobID = job.ID
	task.RepoID = 23
	err = db.Insert(t.Context(), task)
	require.NoError(t, err)

	uploadPackage := func(token string, owner *user_model.User, filename string, expectedStatus int) {
		url := fmt.Sprintf("/api/packages/%s/generic/test-package/1.0/%s.bin", owner.Name, filename)
		req := NewRequestWithBody(t, "PUT", url, bytes.NewReader([]byte{1}))
		if token != "" {
			req.AddTokenAuth(token)
		}
		MakeRequest(t, req, expectedStatus)
	}

	// downloadPackage := func(doer, owner *user_model.User, expectedStatus int) {
	// 	url := fmt.Sprintf("/api/packages/%s/generic/test-package/1.0/admin.bin", owner.Name)
	// 	req := NewRequest(t, "GET", url)
	// 	if doer != nil {
	// 		req.AddBasicAuth(doer.Name)
	// 	}
	// 	MakeRequest(t, req, expectedStatus)
	// }

	type Target struct {
		Owner          *user_model.User
		ExpectedStatus int
	}

	t.Run("Upload", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()

		cases := []struct {
			Token    string
			Filename string
			Targets  []Target
		}{
			{ // Admins can upload to every owner
				Token:    task.Token,
				Filename: "admin",
				Targets: []Target{
					{publicOrgNoMember, http.StatusCreated},
				},
			},
		}

		for _, c := range cases {
			for _, t := range c.Targets {
				uploadPackage(c.Token, t.Owner, c.Filename, t.ExpectedStatus)
			}
		}
	})

	// t.Run("Download", func(t *testing.T) {
	// 	defer tests.PrintCurrentTest(t)()

	// 	cases := []struct {
	// 		Doer     *user_model.User
	// 		Filename string
	// 		Targets  []Target
	// 	}{
	// 		{ // Admins can access everything
	// 			Doer: admin,
	// 			Targets: []Target{
	// 				{admin, http.StatusOK},
	// 				{inactive, http.StatusOK},
	// 				{user, http.StatusOK},
	// 				{limitedUser, http.StatusOK},
	// 				{privateUser, http.StatusOK},
	// 				{privateOrgMember, http.StatusOK},
	// 				{limitedOrgMember, http.StatusOK},
	// 				{publicOrgMember, http.StatusOK},
	// 				{privateOrgNoMember, http.StatusOK},
	// 				{limitedOrgNoMember, http.StatusOK},
	// 				{publicOrgNoMember, http.StatusOK},
	// 			},
	// 		},
	// 		{ // Without credentials only public owners are accessible
	// 			Doer: nil,
	// 			Targets: []Target{
	// 				{admin, http.StatusOK},
	// 				{inactive, http.StatusOK},
	// 				{user, http.StatusOK},
	// 				{limitedUser, http.StatusUnauthorized},
	// 				{privateUser, http.StatusUnauthorized},
	// 				{privateOrgMember, http.StatusUnauthorized},
	// 				{limitedOrgMember, http.StatusUnauthorized},
	// 				{publicOrgMember, http.StatusOK},
	// 				{privateOrgNoMember, http.StatusUnauthorized},
	// 				{limitedOrgNoMember, http.StatusUnauthorized},
	// 				{publicOrgNoMember, http.StatusOK},
	// 			},
	// 		},
	// 		{ // Inactive users have no access
	// 			Doer: inactive,
	// 			Targets: []Target{
	// 				{admin, http.StatusUnauthorized},
	// 				{inactive, http.StatusUnauthorized},
	// 				{user, http.StatusUnauthorized},
	// 				{limitedUser, http.StatusUnauthorized},
	// 				{privateUser, http.StatusUnauthorized},
	// 				{privateOrgMember, http.StatusUnauthorized},
	// 				{limitedOrgMember, http.StatusUnauthorized},
	// 				{publicOrgMember, http.StatusUnauthorized},
	// 				{privateOrgNoMember, http.StatusUnauthorized},
	// 				{limitedOrgNoMember, http.StatusUnauthorized},
	// 				{publicOrgNoMember, http.StatusUnauthorized},
	// 			},
	// 		},
	// 		{ // Normal users can access self, public or limited users/orgs and private orgs in which they are members
	// 			Doer: user,
	// 			Targets: []Target{
	// 				{admin, http.StatusOK},
	// 				{inactive, http.StatusOK},
	// 				{user, http.StatusOK},
	// 				{limitedUser, http.StatusOK},
	// 				{privateUser, http.StatusUnauthorized},
	// 				{privateOrgMember, http.StatusOK},
	// 				{limitedOrgMember, http.StatusOK},
	// 				{publicOrgMember, http.StatusOK},
	// 				{privateOrgNoMember, http.StatusUnauthorized},
	// 				{limitedOrgNoMember, http.StatusOK},
	// 				{publicOrgNoMember, http.StatusOK},
	// 			},
	// 		},
	// 	}

	// 	for _, c := range cases {
	// 		for _, target := range c.Targets {
	// 			downloadPackage(c.Doer, target.Owner, target.ExpectedStatus)
	// 		}
	// 	}
	// })

	// t.Run("API", func(t *testing.T) {
	// 	defer tests.PrintCurrentTest(t)()

	// 	session := loginUser(t, user.Name)
	// 	tokenReadPackage := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeReadPackage)

	// 	for _, target := range []Target{
	// 		{admin, http.StatusOK},
	// 		{inactive, http.StatusOK},
	// 		{user, http.StatusOK},
	// 		{limitedUser, http.StatusOK},
	// 		{privateUser, http.StatusForbidden},
	// 		{privateOrgMember, http.StatusOK},
	// 		{limitedOrgMember, http.StatusOK},
	// 		{publicOrgMember, http.StatusOK},
	// 		{privateOrgNoMember, http.StatusForbidden},
	// 		{limitedOrgNoMember, http.StatusOK},
	// 		{publicOrgNoMember, http.StatusOK},
	// 	} {
	// 		req := NewRequest(t, "GET", "/api/v1/packages/"+target.Owner.Name).
	// 			AddTokenAuth(tokenReadPackage)
	// 		MakeRequest(t, req, target.ExpectedStatus)
	// 	}
	// })
}
