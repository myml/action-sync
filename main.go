package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation"
	"github.com/google/go-github/github"
)

type Config struct {
	Src      string   `json:"src"`
	Dest     string   `json:"dest"`
	Branches []string `json:"branches"`
	Delete   bool     `json:"delete"`
}
type Branch struct {
	Owner  string
	Repo   string
	Branch string
	Base   string
}

var tempBranchRegexp = regexp.MustCompile(`sync/t_\d+`)

func main() {
	privateKey := []byte(os.Getenv("PRIVATE_KEY"))
	var appID, installationID int64
	var files, message string
	var dryRun bool
	var autoMerge bool
	flag.Int64Var(&appID, "app_id", 0, "github app id")
	flag.Int64Var(&installationID, "installation_id", 0, "github installation id")
	flag.StringVar(&message, "message", "chore: Sync by .github", "commit message")
	flag.StringVar(&files, "files", "", "config files, separated by spaces")
	flag.BoolVar(&dryRun, "dryRun", false, "dry run")
	flag.BoolVar(&autoMerge, "autoMerge", true, "auto merge")
	flag.Parse()
	if appID == 0 || installationID == 0 || len(message) == 0 || len(files) == 0 {
		flag.PrintDefaults()
		return
	}

	itr, err := ghinstallation.New(http.DefaultTransport, appID, installationID, []byte(privateKey))
	if err != nil {
		panic(err)
	}
	client := github.NewClient(&http.Client{Transport: itr})
	ctx := context.Background()

	// Sync all repositories if do not repos changed
	for _, file := range strings.Fields(files) {
		data, err := os.ReadFile(file)
		if err != nil {
			log.Fatal(err)
		}
		var configs []Config
		err = json.Unmarshal(data, &configs)
		if err != nil {
			log.Fatal(err)
		}
		log.Println("Config", file)

		mergeBranch := map[string]Branch{}
		cleanupBranch := map[string]Branch{}
		for _, config := range configs {
			owner, repo, path, err := split(config.Dest)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("\tSync %s to %s/%s/%s", config.Src, owner, repo, path)
			if dryRun {
				continue
			}
			// get all branch
			var branches []*github.Branch
			listBranchOpt := github.ListOptions{}
			for {
				bs, resp, err := client.Repositories.ListBranches(ctx, owner, repo, &listBranchOpt)
				if err != nil {
					log.Fatal(err)
				}
				branches = append(branches, bs...)
				if resp.NextPage == 0 {
					break
				}
				listBranchOpt.Page = resp.NextPage
			}
			var syncBranches []string
			// match branch
			if len(config.Branches) == 0 {
				for i := range branches {
					if tempBranchRegexp.MatchString(branches[i].GetName()) {
						continue
					}
					syncBranches = append(syncBranches, branches[i].GetName())
				}
			} else {
				for i := range config.Branches {
					reg := regexp.MustCompile(config.Branches[i])
					match := false
					for j := range branches {
						if reg.Match([]byte(*branches[j].Name)) {
							match = true
							break
						}
					}
					if match {
						syncBranches = append(syncBranches, *branches[i].Name)
					}
				}
			}
			for i := range syncBranches {
				key := fmt.Sprintf("%s/%s/%s", owner, repo, syncBranches[i])
				var branch Branch
				branch, ok := cleanupBranch[key]
				// create temp branch
				if !ok {
					tempBranch := fmt.Sprintf("sync/t_%d/%s", time.Now().Unix(), syncBranches[i])
					tempRef := fmt.Sprintf("refs/heads/%s", tempBranch)
					ref, _, err := client.Git.GetRef(ctx, owner, repo, fmt.Sprintf("heads/%s", syncBranches[i]))
					if err != nil {
						log.Fatal(err)
					}
					ref.Ref = github.String(tempRef)
					_, _, err = client.Git.CreateRef(ctx, owner, repo, ref)
					if err != nil {
						log.Fatal(err)
					}
					branch = Branch{Owner: owner, Repo: repo, Base: syncBranches[i], Branch: tempBranch}
					cleanupBranch[key] = branch
				}
				// put file
				var changed bool
				if config.Delete {
					changed, err = deleteFile(ctx, client, owner, repo, path, message, branch.Branch)
					if err != nil {
						log.Fatal(err)
					}
				} else {
					changed, err = sendFile(ctx, client, config.Src, owner, repo, path, message, branch.Branch)
					if err != nil {
						log.Fatal(err)
					}
				}
				if changed {
					log.Printf("\t\tBranch Sync: %s TempBranch: %s\n", branch.Base, branch.Branch)
					mergeBranch[key] = branch
				} else {
					log.Printf("\t\tBranch No Change: %s TempBranch: %s\n", branch.Base, branch.Branch)
				}
			}
		}

		for _, branch := range mergeBranch {
			pr, _, err := client.PullRequests.Create(ctx, branch.Owner, branch.Repo, &github.NewPullRequest{
				Title:               &message,
				Head:                github.String(branch2Ref(branch.Branch)),
				Base:                github.String(branch2Ref(branch.Base)),
				MaintainerCanModify: github.Bool(true),
			})
			if err != nil {
				log.Println("create pull request: %w", err)
				continue
			}
			if autoMerge {
				_, _, err = client.PullRequests.Merge(ctx,
					branch.Owner, branch.Repo,
					pr.GetNumber(), message,
					&github.PullRequestOptions{SHA: pr.GetHead().GetSHA(), MergeMethod: "squash"},
				)
				if err != nil {
					log.Println("merge pull request: %w", err)
					continue
				}
			}
		}
		if autoMerge {
			for _, branch := range cleanupBranch {
				_, err := client.Git.DeleteRef(ctx, branch.Owner, branch.Repo, branch2Ref(branch.Branch))
				if err != nil {
					log.Println("delete ref faild: %w", err)
				}
			}
		}
	}
}

func branch2Ref(branch string) string {
	return fmt.Sprintf("refs/heads/%s", branch)
}

func split(dest string) (owner, repo, path string, err error) {
	arr := strings.SplitN(dest, "/", 3)
	if len(arr) != 3 {
		return "", "", "", fmt.Errorf("wrong dist. example: owner/repo/file")
	}
	return arr[0], arr[1], arr[2], nil
}

func sendFile(ctx context.Context, client *github.Client, localFile string, owner, repo, path, message string, branch string) (_changed bool, _err error) {
	fileContent, _, resp, err := client.Repositories.GetContents(
		ctx, owner, repo, path,
		&github.RepositoryContentGetOptions{Ref: branch},
	)
	if err != nil {
		if resp.StatusCode != http.StatusNotFound {
			return false, fmt.Errorf("get content: %w", err)
		}
	}
	var latestSha string
	if fileContent != nil {
		latestSha = fileContent.GetSHA()
	}
	content, err := os.ReadFile(localFile)
	if err != nil {
		return false, fmt.Errorf("read file: %w", err)
	}
	sha := sha1.New()
	sha.Write([]byte(fmt.Sprintf("blob %d", len(content))))
	sha.Write([]byte{0})
	sha.Write(content)
	currentSha := hex.EncodeToString(sha.Sum(nil))
	if string(latestSha) == currentSha {
		return false, nil
	}
	_, _, err = client.Repositories.UpdateFile(
		ctx, owner, repo, path,
		&github.RepositoryContentFileOptions{
			Message: &message,
			Content: content,
			SHA:     &latestSha,
			Branch:  &branch,
		},
	)
	if err != nil {
		return false, fmt.Errorf("update file: %w", err)
	}
	return true, nil
}

func deleteFile(ctx context.Context, client *github.Client, owner, repo, path, message, branch string) (_changed bool, _err error) {
	fileContent, _, resp, err := client.Repositories.GetContents(
		ctx, owner, repo, path,
		&github.RepositoryContentGetOptions{Ref: branch},
	)
	if err != nil {
		if resp.StatusCode != http.StatusNotFound {
			return false, fmt.Errorf("get content: %w", err)
		}
		return false, nil
	}
	_, _, err = client.Repositories.DeleteFile(ctx, owner, repo, path, &github.RepositoryContentFileOptions{
		Message: &message,
		SHA:     fileContent.SHA,
		Branch:  &branch,
	})
	if err != nil {
		return false, fmt.Errorf("delete file: %w", err)
	}
	return true, nil
}
