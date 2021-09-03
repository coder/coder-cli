package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"nhooyr.io/websocket"

	"cdr.dev/coder-cli/coder-sdk"
	"cdr.dev/coder-cli/internal/coderutil"
	"cdr.dev/coder-cli/internal/x/xcobra"
	"cdr.dev/coder-cli/pkg/clog"
	"cdr.dev/coder-cli/pkg/tablewriter"
	"cdr.dev/coder-cli/wsnet"

	"github.com/fatih/color"
	"github.com/manifoldco/promptui"
	"github.com/pion/ice/v2"
	"github.com/pion/webrtc/v3"
	"github.com/spf13/cobra"
	"golang.org/x/xerrors"
)

const defaultImgTag = "latest"

func envCmd() *cobra.Command {
	cmd := workspacesCmd()
	cmd.Use = "envs"
	cmd.Deprecated = "use \"workspaces\" instead"
	cmd.Aliases = []string{}
	return cmd
}

func workspacesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workspaces",
		Short:   "Interact with Coder workspaces",
		Long:    "Perform operations on the Coder workspaces owned by the active user.",
		Aliases: []string{"ws"},
	}

	cmd.AddCommand(
		createWorkspaceCmd(),
		editWorkspaceCmd(),
		lsWorkspacesCommand(),
		pingWorkspaceCommand(),
		rebuildWorkspaceCommand(),
		rmWorkspacesCmd(),
		setPolicyTemplate(),
		stopWorkspacesCmd(),
		watchBuildLogCommand(),
		workspaceFromConfigCmd(false),
		workspaceFromConfigCmd(true),
	)
	return cmd
}

const (
	humanOutput = "human"
	jsonOutput  = "json"
)

func lsWorkspacesCommand() *cobra.Command {
	var (
		all       bool
		outputFmt string
		user      string
		provider  string
	)

	cmd := &cobra.Command{
		Use:   "ls",
		Short: "list all workspaces owned by the active user",
		Long:  "List all Coder workspaces owned by the active user.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := newClient(ctx, true)
			if err != nil {
				return err
			}

			var workspaces []coder.Workspace
			switch {
			case all:
				workspaces, err = getAllWorkspaces(ctx, client)
			case provider != "":
				workspaces, err = getWorkspacesByProvider(ctx, client, provider, user)
			default:
				workspaces, err = getWorkspaces(ctx, client, user)
			}
			if err != nil {
				return err
			}
			if len(workspaces) < 1 {
				clog.LogInfo("no workspaces found")
				workspaces = []coder.Workspace{} // ensures that json output still marshals
			}

			switch outputFmt {
			case humanOutput:
				workspaces, err := coderutil.WorkspacesHumanTable(ctx, client, workspaces)
				if err != nil {
					return err
				}
				err = tablewriter.WriteTable(cmd.OutOrStdout(), len(workspaces), func(i int) interface{} {
					return workspaces[i]
				})
				if err != nil {
					return xerrors.Errorf("write table: %w", err)
				}
			case jsonOutput:
				err := json.NewEncoder(cmd.OutOrStdout()).Encode(workspaces)
				if err != nil {
					return xerrors.Errorf("write workspaces as JSON: %w", err)
				}
			default:
				return xerrors.Errorf("unknown --output value %q", outputFmt)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "Get workspaces for all users (admin only)")
	cmd.Flags().StringVar(&user, "user", coder.Me, "Specify the user whose resources to target")
	cmd.Flags().StringVarP(&outputFmt, "output", "o", humanOutput, "human | json")
	cmd.Flags().StringVarP(&provider, "provider", "p", "", "Filter workspaces by a particular workspace provider name.")

	return cmd
}

func pingWorkspaceCommand() *cobra.Command {
	var (
		schemes []string
		count   int
	)

	cmd := &cobra.Command{
		Use:     "ping <workspace_name>",
		Short:   "ping Coder workspaces by name",
		Long:    "ping Coder workspaces by name",
		Example: `coder workspaces ping front-end-workspace`,
		Args:    xcobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := newClient(ctx, true)
			if err != nil {
				return err
			}
			workspace, err := findWorkspace(ctx, client, args[0], coder.Me)
			if err != nil {
				return err
			}

			iceSchemes := map[ice.SchemeType]interface{}{}
			for _, rawScheme := range schemes {
				scheme := ice.NewSchemeType(rawScheme)
				if scheme == ice.Unknown {
					return fmt.Errorf("scheme type %q not recognized", rawScheme)
				}
				iceSchemes[scheme] = nil
			}

			pinger := &wsPinger{
				client:     client,
				workspace:  workspace,
				iceSchemes: iceSchemes,
			}

			seq := 0
			ticker := time.NewTicker(time.Second)
			for {
				select {
				case <-ticker.C:
					err := pinger.ping(ctx)
					if err != nil {
						return err
					}
					seq++
					if count > 0 && seq >= count {
						os.Exit(0)
					}
				case <-ctx.Done():
					return nil
				}
			}
		},
	}

	cmd.Flags().StringSliceVarP(&schemes, "scheme", "s", []string{"stun", "stuns", "turn", "turns"}, "customize schemes to filter ice servers")
	cmd.Flags().IntVarP(&count, "count", "c", 0, "stop after <count> replies")
	return cmd
}

type wsPinger struct {
	client     coder.Client
	workspace  *coder.Workspace
	dialer     *wsnet.Dialer
	iceSchemes map[ice.SchemeType]interface{}
	tunneled   bool
}

func (*wsPinger) logFail(msg string) {
	fmt.Printf("%s: %s\n", color.New(color.Bold, color.FgRed).Sprint("——"), msg)
}

func (*wsPinger) logSuccess(timeStr, msg string) {
	fmt.Printf("%s: %s\n", color.New(color.Bold, color.FgGreen).Sprint(timeStr), msg)
}

// Only return fatal errors.
func (w *wsPinger) ping(ctx context.Context) error {
	ctx, cancelFunc := context.WithTimeout(ctx, time.Second*15)
	defer cancelFunc()
	url := w.client.BaseURL()

	// If the dialer is nil we create a new!
	// nolint:nestif
	if w.dialer == nil {
		servers, err := w.client.ICEServers(ctx)
		if err != nil {
			w.logFail(fmt.Sprintf("list ice servers: %s", err.Error()))
			return nil
		}
		filteredServers := make([]webrtc.ICEServer, 0, len(servers))
		for _, server := range servers {
			good := true
			for _, rawURL := range server.URLs {
				url, err := ice.ParseURL(rawURL)
				if err != nil {
					return fmt.Errorf("parse url %q: %w", rawURL, err)
				}
				if _, ok := w.iceSchemes[url.Scheme]; !ok {
					good = false
				}
			}
			if good {
				filteredServers = append(filteredServers, server)
			}
		}
		if len(filteredServers) == 0 {
			schemes := make([]string, 0)
			for scheme := range w.iceSchemes {
				schemes = append(schemes, scheme.String())
			}
			return fmt.Errorf("no ice servers match the schemes provided: %s", strings.Join(schemes, ","))
		}
		workspace, err := w.client.WorkspaceByID(ctx, w.workspace.ID)
		if err != nil {
			return err
		}
		if workspace.LatestStat.ContainerStatus != coder.WorkspaceOn {
			w.logFail(fmt.Sprintf("workspace is unreachable (status=%s)", workspace.LatestStat.ContainerStatus))
			return nil
		}
		connectStart := time.Now()
		w.dialer, err = wsnet.DialWebsocket(ctx, wsnet.ConnectEndpoint(&url, w.workspace.ID, w.client.Token()), &wsnet.DialOptions{
			ICEServers:         filteredServers,
			TURNProxyAuthToken: w.client.Token(),
			TURNRemoteProxyURL: &url,
			TURNLocalProxyURL:  &url,
		}, &websocket.DialOptions{})
		if err != nil {
			w.logFail(fmt.Sprintf("dial workspace: %s", err.Error()))
			return nil
		}
		connectMS := float64(time.Since(connectStart).Microseconds()) / 1000

		candidates, err := w.dialer.Candidates()
		if err != nil {
			return err
		}
		isRelaying := candidates.Local.Typ == webrtc.ICECandidateTypeRelay
		w.tunneled = false
		candidateURLs := []string{}

		for _, server := range filteredServers {
			if server.Username == wsnet.TURNProxyICECandidate().Username {
				candidateURLs = append(candidateURLs, fmt.Sprintf("turn:%s", url.Host))
				if !isRelaying {
					continue
				}
				w.tunneled = true
				continue
			}

			candidateURLs = append(candidateURLs, server.URLs...)
		}

		connectionText := "direct via STUN"
		if isRelaying {
			connectionText = "proxied via TURN"
		}
		if w.tunneled {
			connectionText = fmt.Sprintf("proxied via %s", url.Host)
		}
		w.logSuccess("——", fmt.Sprintf(
			"connected in %.2fms (%s) candidates=%s",
			connectMS,
			connectionText,
			strings.Join(candidateURLs, ","),
		))
	}

	pingStart := time.Now()
	err := w.dialer.Ping(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			w.dialer = nil
			w.logFail("connection timed out")
			return nil
		}
		if errors.Is(err, webrtc.ErrConnectionClosed) {
			w.dialer = nil
			w.logFail("webrtc connection is closed")
			return nil
		}
		return fmt.Errorf("ping workspace: %w", err)
	}
	pingMS := float64(time.Since(pingStart).Microseconds()) / 1000
	connectionText := "you ↔ workspace"
	if w.tunneled {
		connectionText = fmt.Sprintf("you ↔ %s ↔ workspace", url.Host)
	}
	w.logSuccess(fmt.Sprintf("%.2fms", pingMS), connectionText)
	return nil
}

func stopWorkspacesCmd() *cobra.Command {
	var user string
	cmd := &cobra.Command{
		Use:   "stop [...workspace_names]",
		Short: "stop Coder workspaces by name",
		Long:  "Stop Coder workspaces by name",
		Example: `coder workspaces stop front-end-workspace
coder workspaces stop front-end-workspace backend-workspace

# stop all of your workspaces
coder workspaces ls -o json | jq -c '.[].name' | xargs coder workspaces stop

# stop all workspaces for a given user
coder workspaces --user charlie@coder.com ls -o json \
	| jq -c '.[].name' \
	| xargs coder workspaces --user charlie@coder.com stop`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := newClient(ctx, true)
			if err != nil {
				return xerrors.Errorf("new client: %w", err)
			}

			egroup := clog.LoggedErrGroup()
			for _, workspaceName := range args {
				workspaceName := workspaceName
				egroup.Go(func() error {
					workspace, err := findWorkspace(ctx, client, workspaceName, user)
					if err != nil {
						return err
					}

					if err = client.StopWorkspace(ctx, workspace.ID); err != nil {
						return clog.Error(fmt.Sprintf("stop workspace %q", workspace.Name),
							clog.Causef(err.Error()), clog.BlankLine,
							clog.Hintf("current workspace status is %q", workspace.LatestStat.ContainerStatus),
						)
					}
					clog.LogSuccess(fmt.Sprintf("successfully stopped workspace %q", workspaceName))
					return nil
				})
			}

			return egroup.Wait()
		},
	}
	cmd.Flags().StringVar(&user, "user", coder.Me, "Specify the user whose resources to target")
	return cmd
}

func createWorkspaceCmd() *cobra.Command {
	var (
		org             string
		cpu             float32
		memory          float32
		disk            int
		gpus            int
		img             string
		tag             string
		follow          bool
		useCVM          bool
		providerName    string
		enableAutostart bool
		forUser         string // Optional
	)

	cmd := &cobra.Command{
		Use:   "create [workspace_name]",
		Short: "create a new workspace.",
		Args:  xcobra.ExactArgs(1),
		Long:  "Create a new Coder workspace.",
		Example: `# create a new workspace using default resource amounts
coder workspaces create my-new-workspace --image ubuntu
coder workspaces create my-new-powerful-workspace --cpu 12 --disk 100 --memory 16 --image ubuntu`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if img == "" {
				return xerrors.New("image unset")
			}

			client, err := newClient(ctx, true)
			if err != nil {
				return err
			}

			multiOrgMember, err := isMultiOrgMember(ctx, client, coder.Me)
			if err != nil {
				return err
			}

			if multiOrgMember && org == "" {
				return xerrors.New("org is required for multi-org members")
			}
			importedImg, err := findImg(ctx, client, findImgConf{
				email:   coder.Me,
				imgName: img,
				orgName: org,
			})
			if err != nil {
				return err
			}

			var provider *coder.KubernetesProvider
			if providerName == "" {
				provider, err = coderutil.DefaultWorkspaceProvider(ctx, client)
				if err != nil {
					return xerrors.Errorf("default workspace provider: %w", err)
				}
			} else {
				provider, err = coderutil.ProviderByName(ctx, client, providerName)
				if err != nil {
					return xerrors.Errorf("provider by name: %w", err)
				}
			}

			if forUser != "" && forUser != coder.Me {
				// Making a workspace for another user, do they exist?
				u, err := client.UserByEmail(ctx, forUser)
				if err != nil {
					// Try by ID?
					u, err = client.UserByID(ctx, forUser)
					if err != nil {
						return xerrors.Errorf("the user %q was not found: %w", forUser, err)
					}
				}
				forUser = u.ID
			}

			// ExactArgs(1) ensures our name value can't panic on an out of bounds.
			createReq := &coder.CreateWorkspaceRequest{
				Name:            args[0],
				ImageID:         importedImg.ID,
				OrgID:           importedImg.OrganizationID,
				ImageTag:        tag,
				CPUCores:        cpu,
				MemoryGB:        memory,
				DiskGB:          disk,
				GPUs:            gpus,
				UseContainerVM:  useCVM,
				ResourcePoolID:  provider.ID,
				Namespace:       provider.DefaultNamespace,
				EnableAutoStart: enableAutostart,
				ForUserID:       forUser,
			}

			// if any of these defaulted to their zero value we provision
			// the create request with the imported image defaults instead.
			if createReq.CPUCores == 0 {
				createReq.CPUCores = importedImg.DefaultCPUCores
			}
			if createReq.MemoryGB == 0 {
				createReq.MemoryGB = importedImg.DefaultMemoryGB
			}
			if createReq.DiskGB == 0 {
				createReq.DiskGB = importedImg.DefaultDiskGB
			}

			workspace, err := client.CreateWorkspace(ctx, *createReq)
			if err != nil {
				return xerrors.Errorf("create workspace: %w", err)
			}

			if follow {
				clog.LogSuccess("creating workspace...")
				if err := trailBuildLogs(ctx, client, workspace.ID); err != nil {
					return err
				}
				return nil
			}

			clog.LogSuccess("creating workspace...",
				clog.BlankLine,
				clog.Tipf(`run "coder workspaces watch-build %s" to trail the build logs`, workspace.Name),
			)
			return nil
		},
	}
	cmd.Flags().StringVarP(&org, "org", "o", "", "name of the organization the workspace should be created under.")
	cmd.Flags().StringVarP(&tag, "tag", "t", defaultImgTag, "tag of the image the workspace will be based off of.")
	cmd.Flags().Float32VarP(&cpu, "cpu", "c", 0, "number of cpu cores the workspace should be provisioned with.")
	cmd.Flags().Float32VarP(&memory, "memory", "m", 0, "GB of RAM a workspace should be provisioned with.")
	cmd.Flags().IntVarP(&disk, "disk", "d", 0, "GB of disk storage a workspace should be provisioned with.")
	cmd.Flags().IntVarP(&gpus, "gpus", "g", 0, "number GPUs a workspace should be provisioned with.")
	cmd.Flags().StringVarP(&img, "image", "i", "", "name of the image to base the workspace off of.")
	cmd.Flags().StringVar(&providerName, "provider", "", "name of Workspace Provider with which to create the workspace")
	cmd.Flags().BoolVar(&follow, "follow", false, "follow buildlog after initiating rebuild")
	cmd.Flags().BoolVar(&useCVM, "container-based-vm", false, "deploy the workspace as a Container-based VM")
	cmd.Flags().BoolVar(&enableAutostart, "enable-autostart", false, "automatically start this workspace at your preferred time.")
	cmd.Flags().StringVar(&forUser, "user", coder.Me, "Specify the user whose resources to target")
	_ = cmd.MarkFlagRequired("image")
	return cmd
}

// selectOrg finds the organization in the list or returns the default organization
// if the needle isn't found.
func selectOrg(needle string, haystack []coder.Organization) (*coder.Organization, error) {
	var userOrg *coder.Organization
	for i := range haystack {
		// Look for org by name
		if haystack[i].Name == needle {
			userOrg = &haystack[i]
			break
		}
		// Or use default if the provided is blank
		if needle == "" && haystack[i].Default {
			userOrg = &haystack[i]
			break
		}
	}

	if userOrg == nil {
		if needle != "" {
			return nil, xerrors.Errorf("Unable to locate org '%s'", needle)
		}
		return nil, xerrors.Errorf("Unable to locate a default organization for the user")
	}
	return userOrg, nil
}

// workspaceFromConfigCmd will return a create or an update workspace for a template'd workspace.
// The code for create/update is nearly identical.
// If `update` is true, the update command is returned. If false, the create command.
func workspaceFromConfigCmd(update bool) *cobra.Command {
	var (
		ref           string
		repo          string
		follow        bool
		filepath      string
		org           string
		providerName  string
		workspaceName string
	)

	run := func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		// Update requires the env name, and the name should be the first argument.
		if update {
			workspaceName = args[0]
		} else if workspaceName == "" {
			// Create takes the name as a flag, and it must be set
			return clog.Error("Must provide a workspace name.",
				clog.BlankLine,
				clog.Tipf("Use --name=<workspace-name> to name your workspace"),
			)
		}

		client, err := newClient(ctx, true)
		if err != nil {
			return err
		}

		orgs, err := getUserOrgs(ctx, client, coder.Me)
		if err != nil {
			return err
		}

		multiOrgMember := len(orgs) > 1
		if multiOrgMember && org == "" {
			return xerrors.New("org is required for multi-org members")
		}

		// This is the env to be updated/created
		var workspace *coder.Workspace

		// OrgID is the org where the template and env should be created.
		// If we are updating an env, use the orgID from the workspace.
		var orgID string
		if update {
			workspace, err = findWorkspace(ctx, client, workspaceName, coder.Me)
			if err != nil {
				return handleAPIError(err)
			}
			orgID = workspace.OrganizationID
		} else {
			var userOrg *coder.Organization
			// Select org in list or use default
			userOrg, err := selectOrg(org, orgs)
			if err != nil {
				return err
			}

			orgID = userOrg.ID
		}

		if filepath == "" && ref == "" && repo == "" {
			return clog.Error("Must specify a configuration source",
				"A template source is either sourced from a local file (-f) or from a git repository (--repo-url and --ref)",
			)
		}

		var rd io.Reader
		if filepath != "" {
			b, err := ioutil.ReadFile(filepath)
			if err != nil {
				return xerrors.Errorf("read local file: %w", err)
			}
			rd = bytes.NewReader(b)
		}

		req := coder.ParseTemplateRequest{
			RepoURL:  repo,
			Ref:      ref,
			Local:    rd,
			OrgID:    orgID,
			Filepath: ".coder/coder.yaml",
		}

		version, err := client.ParseTemplate(ctx, req)
		if err != nil {
			return handleAPIError(err)
		}

		provider, err := coderutil.DefaultWorkspaceProvider(ctx, client)
		if err != nil {
			return xerrors.Errorf("default workspace provider: %w", err)
		}

		if update {
			err = client.EditWorkspace(ctx, workspace.ID, coder.UpdateWorkspaceReq{
				TemplateID: &version.TemplateID,
			})
		} else {
			workspace, err = client.CreateWorkspace(ctx, coder.CreateWorkspaceRequest{
				OrgID:          orgID,
				TemplateID:     version.TemplateID,
				ResourcePoolID: provider.ID,
				Namespace:      provider.DefaultNamespace,
				Name:           workspaceName,
			})
		}
		if err != nil {
			return handleAPIError(err)
		}

		if follow {
			clog.LogSuccess("creating workspace...")
			if err := trailBuildLogs(ctx, client, workspace.ID); err != nil {
				return err
			}
			return nil
		}

		clog.LogSuccess("creating workspace...",
			clog.BlankLine,
			clog.Tipf(`run "coder workspaces watch-build %s" to see build logs`, workspace.Name),
		)
		return nil
	}

	var cmd *cobra.Command
	if update {
		cmd = &cobra.Command{
			Use:   "edit-from-config",
			Short: "change the template a workspace is tracking",
			Long:  "Edit an existing Coder workspace using a workspace template.",
			Args:  cobra.ExactArgs(1),
			Example: `# edit a new workspace from git repository
coder workspaces edit-from-config dev-env --repo-url https://github.com/cdr/m --ref my-branch
coder workspaces edit-from-config dev-env --filepath coder.yaml`,
			RunE: run,
		}
	} else {
		cmd = &cobra.Command{
			Use:   "create-from-config",
			Short: "create a new workspace from a template",
			Long:  "Create a new Coder workspace using a workspace template.",
			Example: `# create a new workspace from git repository
coder workspaces create-from-config --name="dev-env" --repo-url https://github.com/cdr/m --ref my-branch
coder workspaces create-from-config --name="dev-env" --filepath coder.yaml`,
			RunE: run,
		}
		cmd.Flags().StringVar(&providerName, "provider", "", "name of Workspace Provider with which to create the workspace")
		cmd.Flags().StringVar(&workspaceName, "name", "", "name of the workspace to be created")
		cmd.Flags().StringVarP(&org, "org", "o", "", "name of the organization the workspace should be created under.")
		// Ref and repo-url can only be used for create
		cmd.Flags().StringVarP(&ref, "ref", "", "master", "git reference to pull template from. May be a branch, tag, or commit hash.")
		cmd.Flags().StringVarP(&repo, "repo-url", "r", "", "URL of the git repository to pull the config from. Config file must live in '.coder/coder.yaml'.")
	}

	cmd.Flags().StringVarP(&filepath, "filepath", "f", "", "path to local template file.")
	cmd.Flags().BoolVar(&follow, "follow", false, "follow buildlog after initiating rebuild")
	return cmd
}

func editWorkspaceCmd() *cobra.Command {
	var (
		org    string
		img    string
		tag    string
		cpu    float32
		memory float32
		disk   int
		gpus   int
		follow bool
		user   string
		force  bool
	)

	cmd := &cobra.Command{
		Use:   "edit",
		Short: "edit an existing workspace and initiate a rebuild.",
		Args:  xcobra.ExactArgs(1),
		Long:  "Edit an existing workspace and initate a rebuild.",
		Example: `coder workspaces edit back-end-workspace --cpu 4

coder workspaces edit back-end-workspace --disk 20`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := newClient(ctx, true)
			if err != nil {
				return err
			}

			workspaceName := args[0]

			workspace, err := findWorkspace(ctx, client, workspaceName, user)
			if err != nil {
				return err
			}

			multiOrgMember, err := isMultiOrgMember(ctx, client, user)
			if err != nil {
				return err
			}

			// if the user belongs to multiple organizations we need them to specify which one.
			if multiOrgMember && org == "" {
				return xerrors.New("org is required for multi-org members")
			}

			req, err := buildUpdateReq(ctx, client, updateConf{
				cpu:       cpu,
				memGB:     memory,
				diskGB:    disk,
				gpus:      gpus,
				workspace: workspace,
				user:      user,
				image:     img,
				imageTag:  tag,
				orgName:   org,
			})
			if err != nil {
				return err
			}

			if !force && workspace.LatestStat.ContainerStatus == coder.WorkspaceOn {
				_, err = (&promptui.Prompt{
					Label:     fmt.Sprintf("Rebuild workspace %q? (will destroy any work outside of your home directory)", workspace.Name),
					IsConfirm: true,
				}).Run()
				if err != nil {
					return clog.Fatal(
						"failed to confirm prompt", clog.BlankLine,
						clog.Tipf(`use "--force" to rebuild without a confirmation prompt`),
					)
				}
			}

			if err := client.EditWorkspace(ctx, workspace.ID, *req); err != nil {
				return xerrors.Errorf("failed to apply changes to workspace %q: %w", workspaceName, err)
			}

			if follow {
				clog.LogSuccess("applied changes to the workspace, rebuilding...")
				if err := trailBuildLogs(ctx, client, workspace.ID); err != nil {
					return err
				}
				return nil
			}

			clog.LogSuccess("applied changes to the workspace, rebuilding...",
				clog.BlankLine,
				clog.Tipf(`run "coder workspaces watch-build %s" to trail the build logs`, workspaceName),
			)
			return nil
		},
	}
	cmd.Flags().StringVarP(&org, "org", "o", "", "name of the organization the workspace should be created under.")
	cmd.Flags().StringVarP(&img, "image", "i", "", "name of the image you want the workspace to be based off of.")
	cmd.Flags().StringVarP(&tag, "tag", "t", "latest", "image tag of the image you want to base the workspace off of.")
	cmd.Flags().Float32VarP(&cpu, "cpu", "c", 0, "The number of cpu cores the workspace should be provisioned with.")
	cmd.Flags().Float32VarP(&memory, "memory", "m", 0, "The amount of RAM a workspace should be provisioned with.")
	cmd.Flags().IntVarP(&disk, "disk", "d", 0, "The amount of disk storage a workspace should be provisioned with.")
	cmd.Flags().IntVarP(&gpus, "gpu", "g", 0, "The amount of disk storage to provision the workspace with.")
	cmd.Flags().BoolVar(&follow, "follow", false, "follow buildlog after initiating rebuild")
	cmd.Flags().StringVar(&user, "user", coder.Me, "Specify the user whose resources to target")
	cmd.Flags().BoolVar(&force, "force", false, "force rebuild without showing a confirmation prompt")
	return cmd
}

func rmWorkspacesCmd() *cobra.Command {
	var (
		force bool
		user  string
	)

	cmd := &cobra.Command{
		Use:   "rm [...workspace_names]",
		Short: "remove Coder workspaces by name",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := newClient(ctx, true)
			if err != nil {
				return err
			}
			if !force {
				confirm := promptui.Prompt{
					Label:     fmt.Sprintf("Delete workspaces %q? (all data will be lost)", args),
					IsConfirm: true,
				}
				if _, err := confirm.Run(); err != nil {
					return clog.Fatal(
						"failed to confirm deletion", clog.BlankLine,
						clog.Tipf(`use "--force" to rebuild without a confirmation prompt`),
					)
				}
			}

			egroup := clog.LoggedErrGroup()
			for _, workspaceName := range args {
				workspaceName := workspaceName
				egroup.Go(func() error {
					workspace, err := findWorkspace(ctx, client, workspaceName, user)
					if err != nil {
						return err
					}
					if err = client.DeleteWorkspace(ctx, workspace.ID); err != nil {
						return clog.Error(
							fmt.Sprintf(`failed to delete workspace "%s"`, workspace.Name),
							clog.Causef(err.Error()),
						)
					}
					clog.LogSuccess(fmt.Sprintf("deleted workspace %q", workspace.Name))
					return nil
				})
			}
			return egroup.Wait()
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "force remove the specified workspaces without prompting first")
	cmd.Flags().StringVar(&user, "user", coder.Me, "Specify the user whose resources to target")
	return cmd
}

type updateConf struct {
	cpu       float32
	memGB     float32
	diskGB    int
	gpus      int
	workspace *coder.Workspace
	user      string
	image     string
	imageTag  string
	orgName   string
}

func buildUpdateReq(ctx context.Context, client coder.Client, conf updateConf) (*coder.UpdateWorkspaceReq, error) {
	var (
		updateReq       coder.UpdateWorkspaceReq
		defaultCPUCores float32
		defaultMemGB    float32
		defaultDiskGB   int
	)

	// If this is not empty it means the user is requesting to change the workspace image.
	if conf.image != "" {
		importedImg, err := findImg(ctx, client, findImgConf{
			email:   conf.user,
			imgName: conf.image,
			orgName: conf.orgName,
		})
		if err != nil {
			return nil, err
		}

		// If the user passes an image arg of the image that
		// the workspace is already using, it was most likely a mistake.
		if conf.image != importedImg.Repository {
			return nil, xerrors.Errorf("workspace is already using image %q", conf.image)
		}

		// Since the workspace image is being changed,
		// the resource amount defaults should be changed to
		// reflect that of the default resource amounts of the new image.
		defaultCPUCores = importedImg.DefaultCPUCores
		defaultMemGB = importedImg.DefaultMemoryGB
		defaultDiskGB = importedImg.DefaultDiskGB
		updateReq.ImageID = &importedImg.ID
	} else {
		// if the workspace image is not being changed, the default
		// resource amounts should reflect the default resource amounts
		// of the image the workspace is already using.
		defaultCPUCores = conf.workspace.CPUCores
		defaultMemGB = conf.workspace.MemoryGB
		defaultDiskGB = conf.workspace.DiskGB
		updateReq.ImageID = &conf.workspace.ImageID
	}

	// The following logic checks to see if the user specified
	// any resource amounts for the workspace that need to be changed.
	// If they did not, then we will get the zero value back
	// and should set the resource amount to the default.

	if conf.cpu == 0 {
		updateReq.CPUCores = &defaultCPUCores
	} else {
		updateReq.CPUCores = &conf.cpu
	}

	if conf.memGB == 0 {
		updateReq.MemoryGB = &defaultMemGB
	} else {
		updateReq.MemoryGB = &conf.memGB
	}

	if conf.diskGB == 0 {
		updateReq.DiskGB = &defaultDiskGB
	} else {
		updateReq.DiskGB = &conf.diskGB
	}

	// Workspace disks can not be shrink so we have to overwrite this
	// if the user accidentally requests it or if the default diskGB value for a
	// newly requested image is smaller than the current amount the workspace is using.
	if *updateReq.DiskGB < conf.workspace.DiskGB {
		clog.LogWarn("disk can not be shrunk",
			fmt.Sprintf("keeping workspace disk at %d GB", conf.workspace.DiskGB),
		)
		updateReq.DiskGB = &conf.workspace.DiskGB
	}

	if conf.gpus != 0 {
		updateReq.GPUs = &conf.gpus
	}

	if conf.imageTag == "" {
		// We're forced to make an alloc here because untyped string consts are not addressable.
		// i.e.  updateReq.ImageTag = &defaultImgTag results in :
		// invalid operation: cannot take address of defaultImgTag (untyped string constant "latest")
		imgTag := defaultImgTag
		updateReq.ImageTag = &imgTag
	} else {
		updateReq.ImageTag = &conf.imageTag
	}
	return &updateReq, nil
}

func setPolicyTemplate() *cobra.Command {
	var (
		ref             string
		repo            string
		filepath        string
		dryRun          bool
		defaultTemplate bool
		scope           string
	)

	cmd := &cobra.Command{
		Use:   "policy-template",
		Short: "Set workspace policy template",
		Long:  "Set workspace policy template or restore to default configuration. This feature is for site admins only.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := newClient(ctx, true)
			if err != nil {
				return err
			}

			if scope != coder.TemplateScopeSite {
				return clog.Error("Invalid 'scope' value", "Valid scope values: site")
			}

			if filepath == "" && !defaultTemplate {
				return clog.Error("Missing required parameter --filepath or --default", "Must specify a template to set")
			}

			templateID := ""
			if filepath != "" {
				var rd io.Reader
				b, err := ioutil.ReadFile(filepath)
				if err != nil {
					return xerrors.Errorf("read local file: %w", err)
				}
				rd = bytes.NewReader(b)

				req := coder.ParseTemplateRequest{
					RepoURL:  repo,
					Ref:      ref,
					Local:    rd,
					OrgID:    coder.SkipTemplateOrg,
					Filepath: ".coder/coder.yaml",
				}

				version, err := client.ParseTemplate(ctx, req)
				if err != nil {
					return handleAPIError(err)
				}
				templateID = version.TemplateID
			}

			resp, err := client.SetPolicyTemplate(ctx, templateID, coder.TemplateScope(scope), dryRun)
			if err != nil {
				return handleAPIError(err)
			}

			for _, mc := range resp.MergeConflicts {
				workspace, err := client.WorkspaceByID(ctx, mc.WorkspaceID)
				if err != nil {
					fmt.Printf("Workspace %q:\n", mc.WorkspaceID)
				} else {
					fmt.Printf("Workspace %q in organization %q:\n", workspace.Name, workspace.OrganizationID)
				}

				fmt.Println(mc.String())
			}

			fmt.Println("Summary:")
			fmt.Println(coder.WorkspaceTemplateMergeConflicts(resp.MergeConflicts).Summary())

			return nil
		},
	}
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "", false, "skip setting policy template, but view errors/warnings about how this policy template would impact existing workspaces")
	cmd.Flags().StringVarP(&filepath, "filepath", "f", "", "full path to local policy template file.")
	cmd.Flags().StringVar(&scope, "scope", "site", "scope of impact for the policy template. Supported values: site")
	cmd.Flags().BoolVar(&defaultTemplate, "default", false, "Restore policy template to default configuration")
	return cmd
}
