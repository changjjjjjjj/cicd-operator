/*
 Copyright 2021 The CI/CD Operator Authors

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package approve

import (
	"fmt"
	"sort"
	"strings"

	cicdv1 "github.com/tmax-cloud/cicd-operator/api/v1"
	"github.com/tmax-cloud/cicd-operator/internal/utils"
	"github.com/tmax-cloud/cicd-operator/pkg/chatops"
	"github.com/tmax-cloud/cicd-operator/pkg/git"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Command types for approve handler
const (
	CommandTypeApprove       = "approve"
	CommandTypeGitLabApprove = "ci-approve"
)

const approvedLabel = "approved"

// Handler is an implementation of both ChatOps Handler and Webhook Plugin for approve
type Handler struct {
	Client client.Client
}

var log = logf.Log.WithName("approve-plugin")

// Name returns a name of the approval plugin
func (h *Handler) Name() string {
	return "approve"
}

// Handle handles a raw webhook
func (h *Handler) Handle(wh *git.Webhook, ic *cicdv1.IntegrationConfig) error {
	// Skip if token is empty
	if ic.Spec.Git.Token == nil {
		return nil
	}

	// Case 1) Approve / Cancel of a pull request (via github/gitlab feature)
	isApproval := wh.EventType == git.EventTypePullRequestReview && wh.IssueComment != nil &&
		wh.IssueComment.Issue.PullRequest.State == git.PullRequestStateOpen && wh.IssueComment.ReviewState != ""

	// Case 2) Label 'approved' is added/deleted
	isLabeled := wh.EventType == git.EventTypePullRequest && wh.PullRequest != nil &&
		(wh.PullRequest.Action == git.PullRequestActionLabeled || wh.PullRequest.Action == git.PullRequestActionUnlabeled)

	// Exit if it's not an approve/cancel action or label action
	if !isApproval && !isLabeled {
		return nil
	}

	gitCli, err := utils.GetGitCli(ic, h.Client)
	if err != nil {
		return err
	}

	// For labeled/unlabeled event
	if isLabeled {
		return h.handleLabelEvent(wh, ic, gitCli)
	}

	// For approve/cancel event
	switch wh.IssueComment.ReviewState {
	case git.PullRequestReviewStateApproved:
		return h.handleApproveCommand(wh.IssueComment, gitCli)
	case git.PullRequestReviewStateUnapproved:
		return h.handleApproveCancelCommand(wh.IssueComment, gitCli)
	}

	return nil
}

// HandleChatOps handles comment commands
func (h *Handler) HandleChatOps(command chatops.Command, webhook *git.Webhook, config *cicdv1.IntegrationConfig) error {
	issueComment := webhook.IssueComment
	// Do nothing if it's not pull request's comment or it's closed
	if issueComment.Issue.PullRequest == nil || issueComment.Issue.PullRequest.State != git.PullRequestStateOpen {
		return nil
	}

	// Skip if token is empty
	if config.Spec.Git.Token == nil {
		return nil
	}

	gitCli, err := utils.GetGitCli(config, h.Client)
	if err != nil {
		return err
	}

	// Authorize or exit
	if err := h.authorize(config, webhook.Sender, issueComment.Issue.PullRequest.Author, gitCli); err != nil {
		unAuthErr, ok := err.(*git.UnauthorizedError)
		if !ok {
			return err
		}

		if err := gitCli.RegisterComment(git.IssueTypePullRequest, issueComment.Issue.PullRequest.ID, generateUserUnauthorizedComment(unAuthErr.User)); err != nil {
			return err
		}
		return nil
	}

	// /approve
	if len(command.Args) == 0 {
		return h.handleApproveCommand(issueComment, gitCli)
	}

	// /approve cancel
	if len(command.Args) == 1 && command.Args[0] == "cancel" {
		return h.handleApproveCancelCommand(issueComment, gitCli)
	}

	// /approve check
	if len(command.Args) == 1 && command.Args[0] == "check" {
		return h.handleApproveCheckCommand(issueComment, gitCli)
	}

	// Default - malformed comment
	if err := gitCli.RegisterComment(git.IssueTypePullRequest, issueComment.Issue.PullRequest.ID, generateHelpComment()); err != nil {
		return err
	}

	return nil
}

// handleLabelEvent handles labeled/unlabeled event for 'approved' label
func (h *Handler) handleLabelEvent(wh *git.Webhook, ic *cicdv1.IntegrationConfig, gitCli git.Client) error {
	pr := wh.PullRequest
	// Check if 'approved' label is set/unset
	isApprovedChanged := false
	for _, l := range pr.LabelChanged {
		if l.Name == approvedLabel {
			isApprovedChanged = true
			break
		}
	}
	if !isApprovedChanged {
		return nil
	}

	log.Info(fmt.Sprintf("%s set/unset approved label on %s/%d", wh.Sender.Name, wh.Repo.URL, wh.PullRequest.ID))

	// Is it set or unset?
	// Can't trust pr's action field (gitlab can set/unset labels at the same time)
	isApprovedLabeled := false
	for _, l := range pr.Labels {
		if l.Name == approvedLabel {
			isApprovedLabeled = true
			break
		}
	}

	// Authorize or exit
	if err := h.authorize(ic, wh.Sender, pr.Author, gitCli); err != nil {
		unAuthErr, ok := err.(*git.UnauthorizedError)
		if !ok {
			return err
		}

		// Set/Unset the label again
		if isApprovedLabeled {
			// Delete approved label
			if err := gitCli.DeleteLabel(git.IssueTypePullRequest, pr.ID, approvedLabel); err != nil && !strings.Contains(err.Error(), "Label does not exist") {
				return err
			}
		} else {
			// Register approved label
			if err := gitCli.SetLabel(git.IssueTypePullRequest, pr.ID, approvedLabel); err != nil {
				return err
			}
		}
		if err := gitCli.RegisterComment(git.IssueTypePullRequest, pr.ID, generateUserUnauthorizedComment(unAuthErr.User)); err != nil {
			return err
		}
		return nil
	}
	return nil
}

// handleApproveCommand handles '/approve' command
func (h *Handler) handleApproveCommand(issueComment *git.IssueComment, gitCli git.Client) error {
	log.Info(fmt.Sprintf("%s approved %s", issueComment.Author.Name, issueComment.Issue.PullRequest.URL))
	// Register approved label
	if err := gitCli.SetLabel(git.IssueTypePullRequest, issueComment.Issue.PullRequest.ID, approvedLabel); err != nil {
		return err
	}

	// Register comment
	if err := gitCli.RegisterComment(git.IssueTypePullRequest, issueComment.Issue.PullRequest.ID, generateApprovedComment(issueComment.Author.Name)); err != nil {
		return err
	}
	return nil
}

// handleApproveCancelCommand handles '/approve cancel] command
func (h *Handler) handleApproveCancelCommand(issueComment *git.IssueComment, gitCli git.Client) error {
	log.Info(fmt.Sprintf("%s canceled approval on %s", issueComment.Author.Name, issueComment.Issue.PullRequest.URL))
	// Delete approved label
	if err := gitCli.DeleteLabel(git.IssueTypePullRequest, issueComment.Issue.PullRequest.ID, approvedLabel); err != nil && !strings.Contains(err.Error(), "Label does not exist") {
		return err
	}

	// Register comment
	if err := gitCli.RegisterComment(git.IssueTypePullRequest, issueComment.Issue.PullRequest.ID, generateApproveCanceledComment(issueComment.Author.Name)); err != nil {
		return err
	}
	return nil
}

func (h *Handler) handleApproveCheckCommand(issueComment *git.IssueComment, gitCli git.Client) error {
	log.Info(fmt.Sprintf("%s check approval status on %s", issueComment.Author.Name, issueComment.Issue.PullRequest.URL))
	// Check approved label
	labels, err := gitCli.ListLabels(issueComment.Issue.PullRequest.ID)
	if err != nil {
		return err
	}
	approveLabel := false
	for _, label := range labels {
		if label.Name == "approved" {
			approveLabel = true
			break
		}
	}
	// Check approved comments
	comments, err := gitCli.ListComments(issueComment.Issue.PullRequest.ID)
	if err != nil {
		return err
	}

	// sort latest comment to oldest comment
	sort.Slice(comments, func(i, j int) bool {
		return comments[j].Comment.CreatedAt.Before(comments[i].Comment.CreatedAt)
	})

	approvedComment := checkApproval(comments)
	// Sync approval label with comments
	if err = h.syncApproval(approveLabel, approvedComment, issueComment, gitCli); err != nil {
		return err
	}
	return nil
}

func (h *Handler) syncApproval(label, comment bool, issueComment *git.IssueComment, gitCli git.Client) error {
	if comment && !label {
		if err := h.handleApproveCommand(issueComment, gitCli); err != nil {
			return err
		}
	}
	if !comment && label {
		if err := h.handleApproveCancelCommand(issueComment, gitCli); err != nil {
			return err
		}
	}
	return nil
}

func checkApproval(comments []git.IssueComment) bool {
	var comment git.IssueComment
	for _, comment = range comments {
		if comment.ReviewState == git.PullRequestReviewStateApproved {
			return true
		}
		if comment.ReviewState == git.PullRequestReviewStateUnapproved {
			return false
		}
		commands := chatops.ExtractCommands(comment.Comment.Body)
		for _, command := range commands {
			if command.Type == "approve" && len(command.Args) == 0 {
				return true
			}
			if command.Type == "approve" && len(command.Args) == 1 && command.Args[0] == "cancel" {
				return false
			}
		}
	}
	return false
}

// authorize decides if the sender is authorized to approve the PR
func (h *Handler) authorize(cfg *cicdv1.IntegrationConfig, sender git.User, author git.User, gitCli git.Client) error {
	// Check if it's PR's author
	if sender.ID == author.ID {
		return &git.UnauthorizedError{User: sender.Name, Repo: cfg.Spec.Git.Repository}
	}

	// Check if it's repo's maintainer
	ok, err := gitCli.CanUserWriteToRepo(sender)
	if err != nil {
		return err
	} else if ok {
		return nil
	}

	return &git.UnauthorizedError{User: sender.Name, Repo: cfg.Spec.Git.Repository}
}

func generateUserUnauthorizedComment(user string) string {
	return fmt.Sprintf("[APPROVE ALERT]\n\nUser `%s` is not allowed to approve/cancel approve this pull request.\n\n"+
		"Users who meet the following conditions can approve the pull request.\n"+
		"- Not an author of the pull request\n"+
		"- (For GitHub) Have write permission on the repository\n"+
		"- (For GitLab) Be Developer, Maintainer, or Owner\n", user)
}

func generateApprovedComment(user string) string {
	return fmt.Sprintf("[APPROVE ALERT]\n\nUser `%s` approved this pull request!", user)
}

func generateApproveCanceledComment(user string) string {
	return fmt.Sprintf("[APPROVE ALERT]\n\nUser `%s` canceled the approval.", user)
}

func generateHelpComment() string {
	return "[APPROVE ALERT]\n\nApprove comment is malformed\n\n" +
		"You can approve or cancel the approve the pull request by commenting...\n" +
		"- (For GitHub) `/approve`\n" +
		"- (For GitHub) `/approve cancel`\n" +
		"- (For GitLab) `/ci-approve`\n" +
		"- (For GitLab) `/ci-approve cancel`\n"
}
