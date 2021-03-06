package gitlab

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	text "github.com/MichaelMure/go-term-text"
	"github.com/pkg/errors"
	"github.com/xanzy/go-gitlab"

	"github.com/MichaelMure/git-bug/bridge/core"
	"github.com/MichaelMure/git-bug/bridge/core/auth"
	"github.com/MichaelMure/git-bug/cache"
	"github.com/MichaelMure/git-bug/entity"
	"github.com/MichaelMure/git-bug/identity"
	"github.com/MichaelMure/git-bug/repository"
	"github.com/MichaelMure/git-bug/util/colors"
)

var (
	ErrBadProjectURL = errors.New("bad project url")
)

func (g *Gitlab) Configure(repo *cache.RepoCache, params core.BridgeParams) (core.Configuration, error) {
	if params.Project != "" {
		fmt.Println("warning: --project is ineffective for a gitlab bridge")
	}
	if params.Owner != "" {
		fmt.Println("warning: --owner is ineffective for a gitlab bridge")
	}

	conf := make(core.Configuration)
	var err error

	if (params.CredPrefix != "" || params.TokenRaw != "") && params.URL == "" {
		return nil, fmt.Errorf("you must provide a project URL to configure this bridge with a token")
	}

	if params.URL == "" {
		params.URL = defaultBaseURL
	}

	var url string

	// get project url
	switch {
	case params.URL != "":
		url = params.URL
	default:
		// terminal prompt
		url, err = promptURL(repo)
		if err != nil {
			return nil, errors.Wrap(err, "url prompt")
		}
	}

	if !strings.HasPrefix(url, params.BaseURL) {
		return nil, fmt.Errorf("base URL (%s) doesn't match the project URL (%s)", params.BaseURL, url)
	}

	user, err := repo.GetUserIdentity()
	if err != nil && err != identity.ErrNoIdentitySet {
		return nil, err
	}

	// default to a "to be filled" user Id if we don't have a valid one yet
	userId := auth.DefaultUserId
	if user != nil {
		userId = user.Id()
	}

	var cred auth.Credential

	switch {
	case params.CredPrefix != "":
		cred, err = auth.LoadWithPrefix(repo, params.CredPrefix)
		if err != nil {
			return nil, err
		}
		if user != nil && cred.UserId() != user.Id() {
			return nil, fmt.Errorf("selected credential don't match the user")
		}
	case params.TokenRaw != "":
		cred = auth.NewToken(userId, params.TokenRaw, target)
	default:
		cred, err = promptTokenOptions(repo, userId)
		if err != nil {
			return nil, err
		}
	}

	token, ok := cred.(*auth.Token)
	if !ok {
		return nil, fmt.Errorf("the Gitlab bridge only handle token credentials")
	}

	// validate project url and get its ID
	id, err := validateProjectURL(params.BaseURL, url, token)
	if err != nil {
		return nil, errors.Wrap(err, "project validation")
	}

	conf[core.ConfigKeyTarget] = target
	conf[keyProjectID] = strconv.Itoa(id)
	conf[keyGitlabBaseUrl] = params.BaseURL

	err = g.ValidateConfig(conf)
	if err != nil {
		return nil, err
	}

	// don't forget to store the now known valid token
	if !auth.IdExist(repo, cred.ID()) {
		err = auth.Store(repo, cred)
		if err != nil {
			return nil, err
		}
	}

	return conf, nil
}

func (g *Gitlab) ValidateConfig(conf core.Configuration) error {
	if v, ok := conf[core.ConfigKeyTarget]; !ok {
		return fmt.Errorf("missing %s key", core.ConfigKeyTarget)
	} else if v != target {
		return fmt.Errorf("unexpected target name: %v", v)
	}

	if _, ok := conf[keyProjectID]; !ok {
		return fmt.Errorf("missing %s key", keyProjectID)
	}

	return nil
}

func promptTokenOptions(repo repository.RepoConfig, userId entity.Id) (auth.Credential, error) {
	for {
		creds, err := auth.List(repo, auth.WithUserId(userId), auth.WithTarget(target), auth.WithKind(auth.KindToken))
		if err != nil {
			return nil, err
		}

		// if we don't have existing token, fast-track to the token prompt
		if len(creds) == 0 {
			value, err := promptToken()
			if err != nil {
				return nil, err
			}
			return auth.NewToken(userId, value, target), nil
		}

		fmt.Println()
		fmt.Println("[1]: enter my token")

		fmt.Println()
		fmt.Println("Existing tokens for Gitlab:")

		sort.Sort(auth.ById(creds))
		for i, cred := range creds {
			token := cred.(*auth.Token)
			fmt.Printf("[%d]: %s => %s (%s)\n",
				i+2,
				colors.Cyan(token.ID().Human()),
				colors.Red(text.TruncateMax(token.Value, 10)),
				token.CreateTime().Format(time.RFC822),
			)
		}

		fmt.Println()
		fmt.Print("Select option: ")

		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Println()
		if err != nil {
			return nil, err
		}

		line = strings.TrimSpace(line)
		index, err := strconv.Atoi(line)
		if err != nil || index < 1 || index > len(creds)+1 {
			fmt.Println("invalid input")
			continue
		}

		switch index {
		case 1:
			value, err := promptToken()
			if err != nil {
				return nil, err
			}
			return auth.NewToken(userId, value, target), nil
		default:
			return creds[index-2], nil
		}
	}
}

func promptToken() (string, error) {
	fmt.Println("You can generate a new token by visiting https://gitlab.com/profile/personal_access_tokens.")
	fmt.Println("Choose 'Create personal access token' and set the necessary access scope for your repository.")
	fmt.Println()
	fmt.Println("'api' access scope: to be able to make api calls")
	fmt.Println()

	re, err := regexp.Compile(`^[a-zA-Z0-9\-]{20}`)
	if err != nil {
		panic("regexp compile:" + err.Error())
	}

	for {
		fmt.Print("Enter token: ")

		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return "", err
		}

		token := strings.TrimSpace(line)
		if re.MatchString(token) {
			return token, nil
		}

		fmt.Println("token has incorrect format")
	}
}

func promptURL(repo repository.RepoCommon) (string, error) {
	// remote suggestions
	remotes, err := repo.GetRemotes()
	if err != nil {
		return "", errors.Wrap(err, "getting remotes")
	}

	validRemotes := getValidGitlabRemoteURLs(remotes)
	if len(validRemotes) > 0 {
		for {
			fmt.Println("\nDetected projects:")

			// print valid remote gitlab urls
			for i, remote := range validRemotes {
				fmt.Printf("[%d]: %v\n", i+1, remote)
			}

			fmt.Printf("\n[0]: Another project\n\n")
			fmt.Printf("Select option: ")

			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil {
				return "", err
			}

			line = strings.TrimSpace(line)

			index, err := strconv.Atoi(line)
			if err != nil || index < 0 || index > len(validRemotes) {
				fmt.Println("invalid input")
				continue
			}

			// if user want to enter another project url break this loop
			if index == 0 {
				break
			}

			return validRemotes[index-1], nil
		}
	}

	// manually enter gitlab url
	for {
		fmt.Print("Gitlab project URL: ")

		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return "", err
		}

		url := strings.TrimSpace(line)
		if url == "" {
			fmt.Println("URL is empty")
			continue
		}

		return url, nil
	}
}

func getProjectPath(projectUrl string) (string, error) {
	cleanUrl := strings.TrimSuffix(projectUrl, ".git")
	cleanUrl = strings.Replace(cleanUrl, "git@", "https://", 1)
	objectUrl, err := url.Parse(cleanUrl)
	if err != nil {
		return "", ErrBadProjectURL
	}

	return objectUrl.Path[1:], nil
}

func getValidGitlabRemoteURLs(remotes map[string]string) []string {
	urls := make([]string, 0, len(remotes))
	for _, u := range remotes {
		path, err := getProjectPath(u)
		if err != nil {
			continue
		}

		urls = append(urls, fmt.Sprintf("%s%s", "gitlab.com", path))
	}

	return urls
}

func validateProjectURL(baseURL, url string, token *auth.Token) (int, error) {
	projectPath, err := getProjectPath(url)
	if err != nil {
		return 0, err
	}

	client, err := buildClient(baseURL, token)
	if err != nil {
		return 0, err
	}

	project, _, err := client.Projects.GetProject(projectPath, &gitlab.GetProjectOptions{})
	if err != nil {
		return 0, err
	}

	return project.ID, nil
}
