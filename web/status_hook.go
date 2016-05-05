package web

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/lgtmco/lgtm/model"
	"github.com/lgtmco/lgtm/remote"
	log "github.com/Sirupsen/logrus"
	"github.com/hashicorp/go-version"
	"regexp"
	"time"
)

type StatusResponse struct {
	SHA     *string `json:"sha,omitempty"`
	Version *string `json:"version,omitempty"`
}

func processStatusHook(c *gin.Context, hook *model.StatusHook) {
	repo, user, err := getRepoAndUser(c, hook.Repo.Slug)
	if err != nil {
		return
	}

	config, maintainer, err := getConfigAndMaintainers(c, user, repo)
	if err != nil {
		return
	}

	if !config.DoMerge {
		c.IndentedJSON(200, gin.H{
		})
		return
	}

	merged := map[string]StatusResponse{}

	pullRequests, err := remote.GetPullRequestsForCommit(c, user, hook.Repo, &hook.SHA)
	log.Debugf("sha for commit is %s, pull requests are: %s", hook.SHA, pullRequests)

	if err != nil {
		log.Errorf("Error while getting pull requests for commit %s %s", hook.SHA, err)
		c.String(500, "Error while getting pull requests for commit %s %s", hook.SHA, err)
		return
	}
	//check the statuses of all of the checks on the branches for this commit
	for _, v := range pullRequests {
		//if all of the statuses are success, then merge and create a tag for the version
		if v.Branch.BranchStatus == "success" && v.Branch.Mergeable {
			sha, err := remote.MergePR(c, user, hook.Repo, v)
			if err != nil {
				log.Warnf("Unable to merge pull request %s: %s", v.Title, err)
				continue
			} else {
				log.Debugf("Merged pull request %s", v.Title)
			}

			merged[v.Title] = StatusResponse{
				SHA: sha,
			}

			if !config.DoVersion {
				continue
			}

			var verStr *string

			switch config.VersionAlg {
			case "timestamp":
				verStr, err = handleTimestamp(config)
			case "semver":
				verStr, err = handleSemver(c, user, hook, v, config, maintainer, repo)
			default:
				log.Warnf("Should have had a valid version algorithm configed at this point -- using semver")
				verStr, err = handleSemver(c, user, hook, v, config, maintainer, repo)

			}
			if config.VersionAlg == "semver" {
				verStr, err = handleSemver(c, user, hook, v, config, maintainer, repo)
			}
			if err != nil {
				continue
			}
			err = remote.Tag(c, user, repo, verStr, sha)
			if err != nil {
				log.Warnf("Unable to tag branch %s: %s", v.Title, err)
				continue
			}
			result := merged[v.Title]
			result.Version = verStr
		}
	}
	log.Debugf("processed status for %s. received %v ", repo.Slug, hook)

	c.IndentedJSON(200, gin.H{
		"merged":    merged,
	})
}

func handleTimestamp(config *model.Config) (*string, error) {
	/*
	All times are in UTC
	Valid format strings:
	- A standard Go format string
	- blank or rfc3339: RFC 3339 format
	- millis: milliseconds since the epoch
	 */
	curTime := time.Now().UTC()
	var format string
	switch config.VersionFormat {
	case "millis":
		//special case, return from here
		out := fmt.Sprintf("%d", curTime.Unix())
		return &out, nil
	case "rfc3339", "":
		format = time.RFC3339
	default:
		format = config.VersionFormat
	}
	out := curTime.Format(format)
	return &out, nil
}

func handleSemver(c *gin.Context, user *model.User, hook *model.StatusHook, pr model.PullRequest, config *model.Config, maintainer *model.Maintainer, repo *model.Repo) (*string, error) {
	// to create the version, need to scan the comments on the pull request to see if anyone specified a version #
	// if so, use the largest specified version #. if not, increment the last version version # for the release
	tags, err := remote.ListTags(c, user, hook.Repo)
	if err != nil {
		log.Warnf("Unable to list tags for %s/%s: %s", hook.Repo.Owner, hook.Repo.Name, err)
	}
	maxVer := getMaxExistingTag(tags)

	comments, err := getComments(c, user, repo, pr.Number)
	if err != nil {
		log.Warnf("Unable to find the comments for pull request %s: %s", pr.Title, err)
		return nil, err
	}

	foundVersion := getMaxVersionComment(config, maintainer, pr.Issue, comments)

	if foundVersion != nil && foundVersion.GreaterThan(maxVer) {
		maxVer = foundVersion
	} else {
		maxParts := maxVer.Segments()
		maxVer, _ = version.NewVersion(fmt.Sprintf("%d.%d.%d", maxParts[0], maxParts[1], maxParts[2] + 1))
	}

	verStr := maxVer.String()
	return &verStr, nil
}

// getMaxVersionComment is a helper function that analyzes the list of comments
// and returns the maximum version found in a comment. if no matching comment is found,
// the function returns version 0.0.0
func getMaxVersionComment(config *model.Config, maintainer *model.Maintainer, issue model.Issue, comments []*model.Comment) *version.Version {
	approverm := map[string]bool{}
	approvers := []*model.Person{}

	maxVersion, _ := version.NewVersion("0.0.0")

	matcher, err := regexp.Compile(config.Pattern)
	if err != nil {
		// this should never happen
		return maxVersion
	}

	for _, comment := range comments {
		// cannot lgtm your own pull request
		if config.SelfApprovalOff && comment.Author == issue.Author {
			continue
		}
		// the user must be a valid maintainer of the project
		person, ok := maintainer.People[comment.Author]
		if !ok {
			continue
		}
		// the same author can't approve something twice
		if _, ok := approverm[comment.Author]; ok {
			continue
		}
		// verify the comment matches the approval pattern
		m := matcher.FindStringSubmatch(comment.Body)
		if len(m) > 0 {
			approverm[comment.Author] = true
			approvers = append(approvers, person)

			if len(m) > 1 {
				//has a version
				curVersion, err := version.NewVersion(m[1])
				if err != nil {
					continue
				}
				if curVersion.GreaterThan(maxVersion) {
					maxVersion = curVersion
				}
			}
		}
	}

	return maxVersion
}

// getMaxExistingTag is a helper function that scans all passed-in tags for a
// comments with semantic versions. It returns the max version found. If no version
// is found, the function returns a version with the value 0.0.0
func getMaxExistingTag(tags []model.Tag) *version.Version {
	//find the previous largest semver value
	maxVer, _ := version.NewVersion("v0.0.0")

	for _, v := range tags {
		curVer, err := version.NewVersion(string(v))
		if err != nil {
			continue
		}
		if curVer.GreaterThan(maxVer) {
			maxVer = curVer
		}
	}

	log.Debugf("maxVer found is %s", maxVer.String())
	return maxVer
}