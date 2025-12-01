package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/alecthomas/kong"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/google/go-github/github"
	"github.com/lmittmann/tint"
	"golang.org/x/oauth2"
)

type CLI struct {
	GithubToken      string   `required:"" env:"GITHUB_TOKEN" help:"Github token"`
	Organisation     string   `required:"" help:"Organisation name" env:"GITHUB_ORGANISATION"`
	AddMinCommits    int      `default:"5" help:"Minimum number of commits to be considered active" env:"GOM_ADD_MIN_COMMITS"`
	AddTimeWindow    int      `default:"1" help:"Time window in years to consider active" env:"GOM_ADD_TIME_WINDOW"`
	RemoveTimeWindow int      `default:"5" help:"Time window in years to consider inactive" env:"GOM_REMOVE_TIME_WINDOW"`
	AlsoRepos        []string `help:"Also consider these repositories" env:"GOM_ALSO_REPOS"`
	IgnoreUsers      []string `help:"Make no recommendation about these users" env:"GOM_IGNORE_USERS"`
	Verbose          bool     `help:"Show what's going on" env:"GOM_VERBOSE"`
}

type member struct {
	login     string
	orgAdmin  bool
	repoAdmin bool
}

func main() {
	var cli CLI
	kong.Parse(&cli)

	tc := oauth2.NewClient(context.Background(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cli.GithubToken}))
	client := github.NewClient(tc)

	level := slog.LevelInfo
	if cli.Verbose {
		level = slog.LevelDebug
	}

	slog.SetDefault(slog.New(
		tint.NewHandler(os.Stdout, &tint.Options{
			Level:      level,
			TimeFormat: time.DateTime,
		}),
	))

	slog.Debug("Listing current members...")
	cur, err := getOrgMembers(client, cli.Organisation)
	if err != nil {
		slog.Error("Failed to list members", "error", err)
		os.Exit(1)
	}
	memberLookup := make(map[string]member)
	memberLogins := make([]string, len(cur))
	admins := mapset.NewSet[string]()
	for i, m := range cur {
		memberLookup[m.login] = m
		memberLogins[i] = m.login
		if m.orgAdmin {
			admins.Add(m.login)
		}
	}
	members := mapset.NewSet(memberLogins...)
	slog.Debug("Listing repositories...")
	repos, err := getRepositoriesByOrg(client, cli.Organisation)
	if err != nil {
		slog.Error("Failed to list repositories", "error", err)
		os.Exit(1)
	}

	// How far back to look for commits for the purpose of adding a member
	// to the organisation.
	cutoff1 := time.Now().AddDate(-cli.AddTimeWindow, 0, 0)

	// How far back to look for commits for the purpose of removing a member
	// from the organisation.
	cutoff2 := time.Now().AddDate(-cli.RemoveTimeWindow, 0, 0)

	interval1Active := mapset.NewSet[string]()
	interval2Active := mapset.NewSet[string]()
	lastCommit := make(map[string]time.Time)
	results := make(chan *comitters)
	var doneWg sync.WaitGroup
	processRepo := func(owner, repo string) {
		slog.Debug("Listing commits...", "owner", owner, "repo", repo)
		coms, err := getRepoCommiters(client, owner, repo, cutoff1, cutoff2)
		if err != nil {
			slog.Error("Failed to list commits", "owner", owner, "repo", repo, "error", err)
			os.Exit(1)
		}
		results <- coms
	}
	for _, also := range cli.AlsoRepos {
		owner, repo, ok := strings.Cut(also, "/")
		if !ok {
			slog.Error("Invalid repository name", "name", also)
			os.Exit(1)
		}
		doneWg.Add(1)
		go func() {
			defer doneWg.Done()
			processRepo(owner, repo)
		}()
	}
	for _, repo := range repos {
		doneWg.Add(1)
		repo := repo
		go func() {
			defer doneWg.Done()
			processRepo(cli.Organisation, repo)
		}()
	}
	go func() {
		doneWg.Wait()
		close(results)
	}()

	intv1Activity := make(map[string]int)
	intv2Activity := make(map[string]int)

	for coms := range results {
		for user, count := range coms.interval1 {
			intv1Activity[user] += count
			if count >= cli.AddMinCommits {
				interval1Active.Add(user)
			}
		}
		for user, count := range coms.interval2 {
			intv2Activity[user] += count
			interval2Active.Add(user)
		}
		for user, date := range coms.lastCommit {
			if lastCommit[user].Before(date) {
				lastCommit[user] = date
			}
		}
	}

	recommendation := false
	add := interval1Active.Difference(members)
	add.RemoveAll(cli.IgnoreUsers...)
	if add.Cardinality() != 0 {
		recommendation = true
		fmt.Println("Add the following members:")
		add.Each(func(user string) bool {
			fmt.Println("+", user)
			return false
		})
	}

	remove := members.Difference(interval2Active)
	remove = remove.Difference(admins)
	remove.RemoveAll(cli.IgnoreUsers...)

	// See if any are repo admins...
	slog.Debug("Checking user permissions...")
nextUser:
	for user, cur := range memberLookup {
		if cur.orgAdmin || cur.repoAdmin {
			continue
		}
		for _, repo := range repos {
			perm, _, err := client.Repositories.GetPermissionLevel(context.Background(), cli.Organisation, repo, user)
			if err != nil {
				slog.Warn("Couldn't get permission level", "user", user, "repo", repo, "error", err)
				continue nextUser
			}
			if perm.GetPermission() == "admin" {
				cur.repoAdmin = true
				memberLookup[user] = cur
				remove.Remove(user)
				continue nextUser
			}
		}
	}

	if remove.Cardinality() != 0 {
		recommendation = true
		fmt.Println("Remove the following members:")
		remove.Each(func(user string) bool {
			fmt.Printf("- %s (last commit %s)\n", user, lastCommit[user].Format(time.DateOnly))
			return false
		})
	}

	// Table of users with commits
	allUsers := members.Union(interval1Active)
	allUsers.RemoveAll(cli.IgnoreUsers...)
	us := allUsers.ToSlice()
	slices.SortFunc(us, func(a, b string) int {
		return cmp.Or(
			-cmp.Compare(intv1Activity[a], intv1Activity[b]),
			-cmp.Compare(intv2Activity[a], intv2Activity[b]),
		)
	})

	fmt.Println("---")
	tw := tabwriter.NewWriter(os.Stdout, 2, 2, 2, ' ', 0)
	for _, u := range us {
		r, s := "", ""
		if memberLookup[u].orgAdmin {
			s = "O"
		} else if memberLookup[u].repoAdmin {
			s = "R"
		} else if intv1Activity[u] >= cli.AddMinCommits {
			s = "*" // active in the last interval (1y)
		} else if intv2Activity[u] >= cli.AddMinCommits {
			s = "?" // active in the long interval (5y)
			r = lastCommit[u].AddDate(cli.RemoveTimeWindow, 0, 0).Format(time.DateOnly)
		} else {
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%4d\t%4d\t%s\t%s\n", s, u, intv1Activity[u], intv2Activity[u], lastCommit[u].Format(time.DateOnly), r)
	}
	_ = tw.Flush()

	if recommendation {
		os.Exit(1)
	}
}

func getOrgMembers(client *github.Client, org string) ([]member, error) {
	var members []member

	// Current org members
	var opt github.ListMembersOptions
	opt.PerPage = 100
	for {
		user, resp, err := client.Organizations.ListMembers(context.Background(), org, &opt)
		for _, u := range user {
			m, _, err := client.Organizations.GetOrgMembership(context.Background(), u.GetLogin(), org)
			if err != nil {
				return nil, err
			}
			members = append(members, member{login: u.GetLogin(), orgAdmin: m.GetRole() == "admin"})
		}
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	// Current outstanding invitations (we expect these will always fit on a
	// single page)
	invs, resp, err := client.Organizations.ListPendingOrgInvitations(context.Background(), org, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	for _, inv := range invs {
		if login := inv.GetLogin(); login != "" {
			members = append(members, member{login: login})
		}
	}

	return members, nil
}

type comitters struct {
	interval1  map[string]int
	interval2  map[string]int
	lastCommit map[string]time.Time
}

func getRepoCommiters(client *github.Client, org, repo string, cutoff1, cutoff2 time.Time) (*comitters, error) {
	var opt github.CommitsListOptions
	opt.PerPage = 100
	res := &comitters{
		interval1:  make(map[string]int),
		interval2:  make(map[string]int),
		lastCommit: make(map[string]time.Time),
	}
	for {
		commit, resp, err := client.Repositories.ListCommits(context.Background(), org, repo, &opt)
		if err != nil {
			slog.Error("Failed to list commits", "owner", org, "repo", repo, "error", err)
			os.Exit(1)
		}
		for _, c := range commit {
			author := c.GetAuthor()
			if author == nil {
				continue
			}
			login := author.GetLogin()
			if strings.Contains(login, "[bot]") {
				continue
			}
			date := c.Commit.GetAuthor().GetDate()
			if date.After(cutoff1) {
				res.interval1[login]++
			}
			if date.After(cutoff2) {
				res.interval2[login]++
			}
			if res.lastCommit[author.GetLogin()].Before(date) {
				res.lastCommit[login] = date
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return res, nil
}

func getRepositoriesByOrg(client *github.Client, org string) ([]string, error) {
	var repositories []string
	var opt github.RepositoryListByOrgOptions
	opt.PerPage = 100
	opt.Type = "public"
	for {
		repo, resp, err := client.Repositories.ListByOrg(context.Background(), org, &opt)
		if err != nil {
			return nil, err
		}
		for _, r := range repo {
			if r.GetArchived() {
				continue
			}
			repositories = append(repositories, r.GetName())
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return repositories, nil
}
