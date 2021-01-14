package cmd

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"cdr.dev/coder-cli/pkg/clog"

	"cdr.dev/coder-cli/coder-sdk"
	"cdr.dev/coder-cli/internal/coderutil"
	"cdr.dev/coder-cli/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/xerrors"
)

const sshStartToken = "# ------------START-CODER-ENTERPRISE-----------"
const sshStartMessage = `# The following has been auto-generated by "coder config-ssh"
# to make accessing your Coder Enterprise environments easier.
#
# To remove this blob, run:
#
#    coder config-ssh --remove
#
# You should not hand-edit this section, unless you are deleting it.`
const sshEndToken = "# ------------END-CODER-ENTERPRISE------------"

func configSSHCmd() *cobra.Command {
	var (
		configpath string
		remove     = false
	)

	cmd := &cobra.Command{
		Use:   "config-ssh",
		Short: "Configure SSH to access Coder environments",
		Long:  "Inject the proper OpenSSH configuration into your local SSH config file.",
		RunE:  configSSH(&configpath, &remove),
	}
	cmd.Flags().StringVar(&configpath, "filepath", filepath.Join("~", ".ssh", "config"), "overide the default path of your ssh config file")
	cmd.Flags().BoolVar(&remove, "remove", false, "remove the auto-generated Coder Enterprise ssh config")

	return cmd
}

func configSSH(configpath *string, remove *bool) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		usr, err := user.Current()
		if err != nil {
			return xerrors.Errorf("get user home directory: %w", err)
		}

		privateKeyFilepath := filepath.Join(usr.HomeDir, ".ssh", "coder_enterprise")

		if strings.HasPrefix(*configpath, "~") {
			*configpath = strings.Replace(*configpath, "~", usr.HomeDir, 1)
		}

		currentConfig, err := readStr(*configpath)
		if os.IsNotExist(err) {
			// SSH configs are not always already there.
			currentConfig = ""
		} else if err != nil {
			return xerrors.Errorf("read ssh config file %q: %w", *configpath, err)
		}

		currentConfig, didRemoveConfig := removeOldConfig(currentConfig)
		if *remove {
			if !didRemoveConfig {
				return xerrors.Errorf("the Coder Enterprise ssh configuration section could not be safely deleted or does not exist")
			}

			err = writeStr(*configpath, currentConfig)
			if err != nil {
				return xerrors.Errorf("write to ssh config file %q: %s", *configpath, err)
			}

			return nil
		}

		client, err := newClient(ctx)
		if err != nil {
			return err
		}

		user, err := client.Me(ctx)
		if err != nil {
			return xerrors.Errorf("fetch username: %w", err)
		}

		envs, err := getEnvs(ctx, client, coder.Me)
		if err != nil {
			return err
		}
		if len(envs) < 1 {
			return xerrors.New("no environments found")
		}

		envsWithPools, err := coderutil.EnvsWithPool(ctx, client, envs)
		if err != nil {
			return xerrors.Errorf("resolve env pools: %w", err)
		}

		if !sshAvailable(envsWithPools) {
			return xerrors.New("SSH is disabled or not available for any environments in your Coder Enterprise deployment.")
		}

		newConfig := makeNewConfigs(user.Username, envsWithPools, privateKeyFilepath)

		err = os.MkdirAll(filepath.Dir(*configpath), os.ModePerm)
		if err != nil {
			return xerrors.Errorf("make configuration directory: %w", err)
		}
		err = writeStr(*configpath, currentConfig+newConfig)
		if err != nil {
			return xerrors.Errorf("write new configurations to ssh config file %q: %w", *configpath, err)
		}
		err = writeSSHKey(ctx, client, privateKeyFilepath)
		if err != nil {
			if !xerrors.Is(err, os.ErrPermission) {
				return xerrors.Errorf("write ssh key: %w", err)
			}
			fmt.Printf("Your private ssh key already exists at \"%s\"\nYou may need to remove the existing private key file and re-run this command\n\n", privateKeyFilepath)
		} else {
			fmt.Printf("Your private ssh key was written to \"%s\"\n", privateKeyFilepath)
		}

		writeSSHUXState(ctx, client, user.ID, envs)
		fmt.Printf("An auto-generated ssh config was written to \"%s\"\n", *configpath)
		fmt.Println("You should now be able to ssh into your environment")
		fmt.Printf("For example, try running\n\n\t$ ssh coder.%s\n\n", envs[0].Name)
		return nil
	}
}

// removeOldConfig removes the old ssh configuration from the user's sshconfig.
// Returns true if the config was modified.
func removeOldConfig(config string) (string, bool) {
	startIndex := strings.Index(config, sshStartToken)
	endIndex := strings.Index(config, sshEndToken)

	if startIndex == -1 || endIndex == -1 {
		return config, false
	}
	if startIndex == 0 {
		return config[endIndex+len(sshEndToken)+1:], true
	}
	return config[:startIndex-1] + config[endIndex+len(sshEndToken)+1:], true
}

// sshAvailable returns true if SSH is available for at least one environment.
func sshAvailable(envs []coderutil.EnvWithPool) bool {
	for _, env := range envs {
		if env.Pool.SSHEnabled {
			return true
		}
	}
	return false
}

func writeSSHKey(ctx context.Context, client *coder.Client, privateKeyPath string) error {
	key, err := client.SSHKey(ctx)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(privateKeyPath, []byte(key.PrivateKey), 0400)
}

func makeNewConfigs(userName string, envs []coderutil.EnvWithPool, privateKeyFilepath string) string {
	newConfig := fmt.Sprintf("\n%s\n%s\n\n", sshStartToken, sshStartMessage)
	for _, env := range envs {
		if !env.Pool.SSHEnabled {
			continue
		}
		u, err := url.Parse(env.Pool.AccessURL)
		if err != nil {
			clog.LogWarn("invalid access url", clog.Causef("malformed url: %q", env.Pool.AccessURL))
			continue
		}
		newConfig += makeSSHConfig(u.Host, userName, env.Env.Name, privateKeyFilepath)
	}
	newConfig += fmt.Sprintf("\n%s\n", sshEndToken)

	return newConfig
}

func makeSSHConfig(host, userName, envName, privateKeyFilepath string) string {
	return fmt.Sprintf(
		`Host coder.%s
   HostName %s
   User %s-%s
   StrictHostKeyChecking no
   ConnectTimeout=0
   IdentityFile="%s"
   ServerAliveInterval 60
   ServerAliveCountMax 3
`, envName, host, userName, envName, privateKeyFilepath)
}

func configuredHostname() (string, error) {
	u, err := config.URL.Read()
	if err != nil {
		return "", err
	}
	url, err := url.Parse(u)
	if err != nil {
		return "", err
	}
	return url.Hostname(), nil
}

func writeStr(filename, data string) error {
	return ioutil.WriteFile(filename, []byte(data), 0777)
}

func readStr(filename string) (string, error) {
	contents, err := ioutil.ReadFile(filename)
	if err != nil {
		return "", err
	}
	return string(contents), nil
}

func writeSSHUXState(ctx context.Context, client *coder.Client, userID string, envs []coder.Environment) {
	// Create a map of env.ID -> true to indicate to the web client that all
	// current environments have SSH configured
	cliSSHConfigured := make(map[string]bool)
	for _, env := range envs {
		cliSSHConfigured[env.ID] = true
	}
	// Update UXState that coder config-ssh has been run by the currently
	// authenticated user
	err := client.UpdateUXState(ctx, userID, map[string]interface{}{"cliSSHConfigured": cliSSHConfigured})
	if err != nil {
		clog.LogWarn("The Coder web client may not recognize that you've configured SSH.")
	}
}
