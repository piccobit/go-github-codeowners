package codeowners

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar"
	"github.com/google/go-github/v19/github"
	"github.com/rs/zerolog/log"
)

// comms holds the channels that are used for communicating async
type comms struct {
	data chan *github.User
	err  chan error
	wait *sync.WaitGroup
}

// this struct holds the description of a whole codeowners file
type CodeOwners struct {
	owner    string
	repo     string
	patterns []codeOwner
}

// this struct holds a single line from a codeowners file
type codeOwner struct {
	path   string
	owners []string
}

// format a codeOwners struct back into a string
func (co CodeOwners) String() string {
	lines := make([]string, len(co.patterns))
	for idx, owner := range co.patterns {
		lines[idx] = owner.String()
	}
	return strings.Join(lines, "\n")
}

// format a single line of a codeowners file
func (co codeOwner) String() string {
	return fmt.Sprintf("%v %v", co.path, strings.Join(co.owners, " "))
}

var (
	client *github.Client
)

// this will attempt to get the CODEOWNERS file from the various locations in the github repo
func fetch(ctx context.Context, owner string, repo string) (string, error) {
	options := github.RepositoryContentGetOptions{}
	var files [3]string
	files[0] = ""
	files[1] = "docs/"
	files[2] = ".github/"
	var content *github.RepositoryContent
	var err error
	for _, filepath := range files {
		content, _, _, err = client.Repositories.GetContents(ctx, owner, repo, filepath+"CODEOWNERS", &options)

		if err != nil {
			log.Warn().Str("filepath", filepath).Msg("Warning getting code owners file")
			continue
		} else {
			log.Debug().Str("filepath", filepath).Msg("Found CODEOWNERS file")
		}

		return content.GetContent()
	}
	return "", err
}

// takes a username and asks the github api for full information about a user which is sent through the data channel as a github.User struct
func fetchuser(name string, ctx context.Context, ch comms) {
	defer ch.wait.Done()
	user, resp, err := client.Users.Get(ctx, name)
	if err != nil {
		log.Error().Err(err).Interface("resp", resp).Msg("fetchuser")
		ch.err <- err
	} else {
		ch.data <- user
	}
}

// takes an email string, parses it out to ensure validity and then constructs a github.User struct to send back down the data channel
// the github api does not allow for searching by an email address so this is the best that I can manage
func finduseremail(email string, _ context.Context, ch comms) {
	defer ch.wait.Done()
	e, err := mail.ParseAddress(email)
	if err != nil {
		ch.err <- err
		return
	}
	ch.data <- &github.User{
		Email: &e.Address,
	}
}

// this takes a string team name in the form of org/slug and sends the github users back through the data channel
func expandteam(fullteam string, ctx context.Context, ch comms) {
	defer ch.wait.Done()

	var teamid int64

	split := strings.Index(fullteam, "/")
	orga := fullteam[1:split]
	teamname := fullteam[split+1:]
	opts := &github.ListOptions{}

	for {
		teams, resp, err := client.Teams.ListTeams(ctx, orga, opts)

		if err != nil {
			ch.err <- err
			return
		}

		for _, team := range teams {
			if teamname == *team.Slug {
				teamid = *team.ID
				break
			}
		}

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	if teamid == 0 {
		ch.err <- errors.New(fmt.Sprintf("Failed to find team matching %v", teamname))
		return
	}

	opt := github.TeamListTeamMembersOptions{}
	users, _, err := client.Teams.ListTeamMembers(ctx, teamid, &opt)
	if err != nil {
		ch.err <- err
		return
	}
	for _, user := range users {
		ch.wait.Add(1)
		go fetchuser(*user.Login, ctx, ch)
	}
}

// this takes an individual owner (team, email or login) and sends github.User objects to the data channel
func expandowners(ownertext string, ctx context.Context, ch comms) {
	defer ch.wait.Done()
	switch {
	case strings.HasPrefix(ownertext, "@") && strings.Contains(ownertext, "/"):
		ch.wait.Add(1)
		go expandteam(ownertext, ctx, ch)
	case strings.HasPrefix(ownertext, "@"):
		ch.wait.Add(1)
		go fetchuser(ownertext[1:], ctx, ch)
	case strings.Contains(ownertext, "@"):
		ch.wait.Add(1)
		go finduseremail(ownertext, ctx, ch)
	default:
		ch.err <- errors.New(fmt.Sprintf("Do not understand user specification %q", ownertext))
	}
}

// Get is the "entrypoint" where a codeOwners struct is returned for calling Match on
func Get(ctx context.Context, cl *github.Client, owner string, repo string) (CodeOwners, error) {
	client = cl
	obj := CodeOwners{
		owner: owner,
		repo:  repo,
	}
	patterns := make([]codeOwner, 0)
	content, err := fetch(ctx, owner, repo)
	if err != nil {
		return obj, err
	}

	log.Debug().Str("content", content).Msg("fetch")

	for _, line := range strings.Split(content, "\n") {
		words := strings.Fields(line)

		if len(words) > 1 {
			log.Debug().Strs("words", words).Msg("fetch")

			if words[0] == "*" {
				words[0] = "**"
			}
			patterns = append(patterns, codeOwner{
				path:   words[0],
				owners: words[1:],
			})
		}
	}
	obj.patterns = patterns
	return obj, nil
}

// Match a file to some github users (or email addresses)
// called on a codeOwners struct
func (co CodeOwners) Match(ctx context.Context, path string) (users []*github.User, errorSlices []error) {
	var owners []string
	for _, pattern := range co.patterns {
		match, _ := doublestar.Match(pattern.path, path)
		if match {
			owners = pattern.owners

			log.Debug().Interface("owners", owners).Interface("pattern", pattern).Msg("Found match")
		}
	}
	if owners == nil {
		errorSlices = append(errorSlices, errors.New("failed to find owner"))
		return nil, errorSlices
	}
	var wg sync.WaitGroup
	ch := comms{
		data: make(chan *github.User),
		err:  make(chan error),
		wait: &wg,
	}
	for _, ownertext := range owners {
		ch.wait.Add(1)
		go expandowners(ownertext, ctx, ch)
	}
	go func() {
		ch.wait.Wait()
		close(ch.data)
		close(ch.err)
	}()
	errClosed, dataClosed := false, false
	for {
		// If both channels are closed then we can stop
		if errClosed && dataClosed {
			return users, errorSlices
		}
		select {
		case <-ctx.Done():
			return // returning not to leak the goroutine
		case err, errOk := <-ch.err:
			if !errOk {
				errClosed = true
			} else {
				errorSlices = append(errorSlices, err)
			}
		case user, dataOk := <-ch.data:
			if !dataOk {
				dataClosed = true
			} else {
				users = append(users, user)
			}
		}
	}
}
