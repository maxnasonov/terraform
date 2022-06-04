package command

import (
	"bytes"
	"fmt"
	"github.com/hashicorp/terraform/internal/states/statefile"
	"github.com/hashicorp/terraform/internal/states/statemgr"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"
	"github.com/posener/complete"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/backend"
	backendInit "github.com/hashicorp/terraform/internal/backend/init"
	"github.com/hashicorp/terraform/internal/command/arguments"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/terraform"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

// TfversionCommand is a Command implementation that takes a Terraform
// module and clones it to the working directory.
type TfversionCommand struct {
	Meta
}

func (c *TfversionCommand) Run(args []string) int {
	var flagFromModule, flagLockfile string
	var flagBackend, flagCloud, flagGet, flagUpgrade bool
	var flagPluginPath FlagStringSlice
	flagConfigExtra := newRawFlags("-backend-config")

	args = c.Meta.process(args)
	cmdFlags := c.Meta.extendedFlagSet("tfstate")
	cmdFlags.BoolVar(&flagBackend, "backend", true, "")
	cmdFlags.BoolVar(&flagCloud, "cloud", true, "")
	cmdFlags.Var(flagConfigExtra, "backend-config", "")
	cmdFlags.StringVar(&flagFromModule, "from-module", "", "copy the source of the given module into the directory before init")
	cmdFlags.BoolVar(&flagGet, "get", true, "")
	cmdFlags.BoolVar(&c.forceInitCopy, "force-copy", false, "suppress prompts about copying state data")
	cmdFlags.BoolVar(&c.Meta.stateLock, "lock", true, "lock state")
	cmdFlags.DurationVar(&c.Meta.stateLockTimeout, "lock-timeout", 0, "lock timeout")
	cmdFlags.BoolVar(&c.reconfigure, "reconfigure", false, "reconfigure")
	cmdFlags.BoolVar(&c.migrateState, "migrate-state", false, "migrate state")
	cmdFlags.BoolVar(&flagUpgrade, "upgrade", false, "")
	cmdFlags.Var(&flagPluginPath, "plugin-dir", "plugin directory")
	cmdFlags.StringVar(&flagLockfile, "lockfile", "", "Set a dependency lockfile mode")
	cmdFlags.BoolVar(&c.Meta.ignoreRemoteVersion, "ignore-remote-version", false, "continue even if remote and local Terraform versions are incompatible")
	cmdFlags.Usage = func() { c.Ui.Error(c.Help()) }
	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	backendFlagSet := arguments.FlagIsSet(cmdFlags, "backend")
	cloudFlagSet := arguments.FlagIsSet(cmdFlags, "cloud")

	switch {
	case backendFlagSet && cloudFlagSet:
		c.Ui.Error("The -backend and -cloud options are aliases of one another and mutually-exclusive in their use")
		return 1
	case backendFlagSet:
		flagCloud = flagBackend
	case cloudFlagSet:
		flagBackend = flagCloud
	}

	if c.migrateState && c.reconfigure {
		c.Ui.Error("The -migrate-state and -reconfigure options are mutually-exclusive")
		return 1
	}

	// Copying the state only happens during backend migration, so setting
	// -force-copy implies -migrate-state
	if c.forceInitCopy {
		c.migrateState = true
	}

	var diags tfdiags.Diagnostics

	if len(flagPluginPath) > 0 {
		c.pluginPath = flagPluginPath
	}

	// Validate the arg count and get the working directory
	args = cmdFlags.Args()
	path, err := ModulePath(args)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	if err := c.storePluginPath(c.pluginPath); err != nil {
		c.Ui.Error(fmt.Sprintf("Error saving -plugin-path values: %s", err))
		return 1
	}

	if flagFromModule != "" {
		src := flagFromModule

		empty, err := configs.IsEmptyDir(path)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error validating destination directory: %s", err))
			return 1
		}
		if !empty {
			c.Ui.Error(strings.TrimSpace(errTfversionCopyNotEmpty))
			return 1
		}

		c.Ui.Output(c.Colorize().Color(fmt.Sprintf(
			"[reset][bold]Copying configuration[reset] from %q...", src,
		)))

		hooks := uiModuleInstallHooks{
			Ui:             c.Ui,
			ShowLocalPaths: false, // since they are in a weird location for init
		}

		initDirFromModuleAbort, initDirFromModuleDiags := c.initDirFromModule(path, src, hooks)
		diags = diags.Append(initDirFromModuleDiags)
		if initDirFromModuleAbort || initDirFromModuleDiags.HasErrors() {
			c.showDiagnostics(diags)
			return 1
		}

		c.Ui.Output("")
	}

	// If our directory is empty, then we're done. We can't get or set up
	// the backend with an empty directory.
	empty, err := configs.IsEmptyDir(path)
	if err != nil {
		diags = diags.Append(fmt.Errorf("Error checking configuration: %s", err))
		c.showDiagnostics(diags)
		return 1
	}
	if empty {
		c.Ui.Output(c.Colorize().Color(strings.TrimSpace(outputTfversionEmpty)))
		return 0
	}

	// For Terraform v0.12 we introduced a special loading mode where we would
	// use the 0.11-syntax-compatible "earlyconfig" package as a heuristic to
	// identify situations where it was likely that the user was trying to use
	// 0.11-only syntax that the upgrade tool might help with.
	//
	// However, as the language has moved on that is no longer a suitable
	// heuristic in Terraform 0.13 and later: other new additions to the
	// language can cause the main loader to disagree with earlyconfig, which
	// would lead us to give poor advice about how to respond.
	//
	// For that reason, we no longer use a different error message in that
	// situation, but for now we still use both codepaths because some of our
	// initialization functionality remains built around "earlyconfig" and
	// so we need to still load the module via that mechanism anyway until we
	// can do some more invasive refactoring here.
	rootModEarly, earlyConfDiags := c.loadSingleModuleEarly(path)
	// If _only_ the early loader encountered errors then that's unusual
	// (it should generally be a superset of the normal loader) but we'll
	// return those errors anyway since otherwise we'll probably get
	// some weird behavior downstream. Errors from the early loader are
	// generally not as high-quality since it has less context to work with.
	if earlyConfDiags.HasErrors() {
		c.Ui.Error(c.Colorize().Color(strings.TrimSpace(errTfversionConfigError)))
		// Errors from the early loader are generally not as high-quality since
		// it has less context to work with.

		// TODO: It would be nice to check the version constraints in
		// rootModEarly.RequiredCore and print out a hint if the module is
		// declaring that it's not compatible with this version of Terraform,
		// and that may be what caused earlyconfig to fail.
		diags = diags.Append(earlyConfDiags)
		c.showDiagnostics(diags)
		return 1
	}

	if flagGet {
		_, modsAbort, modsDiags := c.getModules(path, rootModEarly, flagUpgrade)
		diags = diags.Append(modsDiags)
		if modsAbort || modsDiags.HasErrors() {
			c.showDiagnostics(diags)
			return 1
		}
	}

	// With all of the modules (hopefully) installed, we can now try to load the
	// whole configuration tree.
	config, confDiags := c.loadConfig(path)
	// configDiags will be handled after the version constraint check, since an
	// incorrect version of terraform may be producing errors for configuration
	// constructs added in later versions.

	// Before we go further, we'll check to make sure none of the modules in
	// the configuration declare that they don't support this Terraform
	// version, so we can produce a version-related error message rather than
	// potentially-confusing downstream errors.
	versionDiags := terraform.CheckCoreVersionRequirements(config)
	if versionDiags.HasErrors() {
		c.showDiagnostics(versionDiags)
		return 1
	}

	diags = diags.Append(confDiags)
	if confDiags.HasErrors() {
		c.Ui.Error(strings.TrimSpace(errTfversionConfigError))
		c.showDiagnostics(diags)
		return 1
	}

	var back backend.Backend

	switch {
	case flagCloud && config.Module.CloudConfig != nil:
		be, _, backendDiags := c.initCloud(config.Module, flagConfigExtra)
		diags = diags.Append(backendDiags)
		if backendDiags.HasErrors() {
			c.showDiagnostics(diags)
			return 1
		}
		back = be
	case flagBackend:
		be, _, backendDiags := c.initBackend(config.Module, flagConfigExtra)
		diags = diags.Append(backendDiags)
		if backendDiags.HasErrors() {
			c.showDiagnostics(diags)
			return 1
		}
		back = be
	default:
		// load the previously-stored backend config
		be, backendDiags := c.Meta.backendFromState()
		diags = diags.Append(backendDiags)
		if backendDiags.HasErrors() {
			c.showDiagnostics(diags)
			return 1
		}
		back = be
	}

	if back == nil {
		// If we didn't initialize a backend then we'll try to at least
		// instantiate one. This might fail if it wasn't already initialized
		// by a previous run, so we must still expect that "back" may be nil
		// in code that follows.
		var backDiags tfdiags.Diagnostics
		back, backDiags = c.Backend(&BackendOpts{Init: true})
		if backDiags.HasErrors() {
			// This is fine. We'll proceed with no backend, then.
			back = nil
		}
	}

	// If we have a functional backend (either just initialized or initialized
	// on a previous run) we'll use the current state as a potential source
	// of provider dependencies.
	if back != nil {
		c.ignoreRemoteVersionConflict(back)
		workspace, err := c.Workspace()
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error selecting workspace: %s", err))
			return 1
		}
		sMgr, err := back.StateMgr(workspace)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error loading state: %s", err))
			return 1
		}

		if sMgr == nil {
			return 0
		}

		if err := sMgr.RefreshState(); err != nil {
			c.Ui.Error(fmt.Sprintf("Error refreshing state: %s", err))
			return 1
		}

		if sMgr.State() != nil {
			// Get a statefile object representing the latest snapshot
			stateFile := statemgr.Export(sMgr)
			if stateFile != nil { // we produce no output if the statefile is nil
				println(stateFile.TerraformVersion.String())
				var buf bytes.Buffer
				err = statefile.Write(stateFile, &buf)
				if err != nil {
					c.Ui.Error(fmt.Sprintf("Failed to write state: %s", err))
					return 1
				}

				//c.Ui.Output(buf.String())
			}

		}
	}
	return 0
}

func (c *TfversionCommand) getModules(path string, earlyRoot *tfconfig.Module, upgrade bool) (output bool, abort bool, diags tfdiags.Diagnostics) {
	if len(earlyRoot.ModuleCalls) == 0 {
		// Nothing to do
		return false, false, nil
	}

	if upgrade {
		c.Ui.Output(c.Colorize().Color("[reset][bold]Upgrading modules..."))
	} else {
		c.Ui.Output(c.Colorize().Color("[reset][bold]Initializing modules..."))
	}

	hooks := uiModuleInstallHooks{
		Ui:             c.Ui,
		ShowLocalPaths: true,
	}

	installAbort, installDiags := c.installModules(path, upgrade, hooks)
	diags = diags.Append(installDiags)

	// At this point, installModules may have generated error diags or been
	// aborted by SIGINT. In any case we continue and the manifest as best
	// we can.

	// Since module installer has modified the module manifest on disk, we need
	// to refresh the cache of it in the loader.
	if c.configLoader != nil {
		if err := c.configLoader.RefreshModules(); err != nil {
			// Should never happen
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Failed to read module manifest",
				fmt.Sprintf("After installing modules, Terraform could not re-read the manifest of installed modules. This is a bug in Terraform. %s.", err),
			))
		}
	}

	return true, installAbort, diags
}

func (c *TfversionCommand) initCloud(root *configs.Module, extraConfig rawFlags) (be backend.Backend, output bool, diags tfdiags.Diagnostics) {
	//c.Ui.Output(c.Colorize().Color("\n[reset][bold]Initializing Terraform Cloud..."))

	if len(extraConfig.AllItems()) != 0 {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid command-line option",
			"The -backend-config=... command line option is only for state backends, and is not applicable to Terraform Cloud-based configurations.\n\nTo change the set of workspaces associated with this configuration, edit the Cloud configuration block in the root module.",
		))
		return nil, true, diags
	}

	backendConfig := root.CloudConfig.ToBackendConfig()

	opts := &BackendOpts{
		Config: &backendConfig,
		Init:   true,
	}

	back, backDiags := c.Backend(opts)
	diags = diags.Append(backDiags)
	return back, true, diags
}

func (c *TfversionCommand) initBackend(root *configs.Module, extraConfig rawFlags) (be backend.Backend, output bool, diags tfdiags.Diagnostics) {
	var backendConfig *configs.Backend
	var backendConfigOverride hcl.Body
	if root.Backend != nil {
		backendType := root.Backend.Type
		if backendType == "cloud" {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Unsupported backend type",
				Detail:   fmt.Sprintf("There is no explicit backend type named %q. To configure Terraform Cloud, declare a 'cloud' block instead.", backendType),
				Subject:  &root.Backend.TypeRange,
			})
			return nil, true, diags
		}

		bf := backendInit.Backend(backendType)
		if bf == nil {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Unsupported backend type",
				Detail:   fmt.Sprintf("There is no backend type named %q.", backendType),
				Subject:  &root.Backend.TypeRange,
			})
			return nil, true, diags
		}

		b := bf()
		backendSchema := b.ConfigSchema()
		backendConfig = root.Backend

		var overrideDiags tfdiags.Diagnostics
		backendConfigOverride, overrideDiags = c.backendConfigOverrideBody(extraConfig, backendSchema)
		diags = diags.Append(overrideDiags)
		if overrideDiags.HasErrors() {
			return nil, true, diags
		}
	} else {
		// If the user supplied a -backend-config on the CLI but no backend
		// block was found in the configuration, it's likely - but not
		// necessarily - a mistake. Return a warning.
		if !extraConfig.Empty() {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Warning,
				"Missing backend configuration",
				`-backend-config was used without a "backend" block in the configuration.

If you intended to override the default local backend configuration,
no action is required, but you may add an explicit backend block to your
configuration to clear this warning:

terraform {
  backend "local" {}
}

However, if you intended to override a defined backend, please verify that
the backend configuration is present and valid.
`,
			))
		}
	}

	opts := &BackendOpts{
		Config:         backendConfig,
		ConfigOverride: backendConfigOverride,
		Init:           true,
	}

	back, backDiags := c.Backend(opts)
	diags = diags.Append(backDiags)
	return back, true, diags
}

// backendConfigOverrideBody interprets the raw values of -backend-config
// arguments into a hcl Body that should override the backend settings given
// in the configuration.
//
// If the result is nil then no override needs to be provided.
//
// If the returned diagnostics contains errors then the returned body may be
// incomplete or invalid.
func (c *TfversionCommand) backendConfigOverrideBody(flags rawFlags, schema *configschema.Block) (hcl.Body, tfdiags.Diagnostics) {
	items := flags.AllItems()
	if len(items) == 0 {
		return nil, nil
	}

	var ret hcl.Body
	var diags tfdiags.Diagnostics
	synthVals := make(map[string]cty.Value)

	mergeBody := func(newBody hcl.Body) {
		if ret == nil {
			ret = newBody
		} else {
			ret = configs.MergeBodies(ret, newBody)
		}
	}
	flushVals := func() {
		if len(synthVals) == 0 {
			return
		}
		newBody := configs.SynthBody("-backend-config=...", synthVals)
		mergeBody(newBody)
		synthVals = make(map[string]cty.Value)
	}

	if len(items) == 1 && items[0].Value == "" {
		// Explicitly remove all -backend-config options.
		// We do this by setting an empty but non-nil ConfigOverrides.
		return configs.SynthBody("-backend-config=''", synthVals), diags
	}

	for _, item := range items {
		eq := strings.Index(item.Value, "=")

		if eq == -1 {
			// The value is interpreted as a filename.
			newBody, fileDiags := c.loadHCLFile(item.Value)
			diags = diags.Append(fileDiags)
			if fileDiags.HasErrors() {
				continue
			}
			// Generate an HCL body schema for the backend block.
			var bodySchema hcl.BodySchema
			for name := range schema.Attributes {
				// We intentionally ignore the `Required` attribute here
				// because backend config override files can be partial. The
				// goal is to make sure we're not loading a file with
				// extraneous attributes or blocks.
				bodySchema.Attributes = append(bodySchema.Attributes, hcl.AttributeSchema{
					Name: name,
				})
			}
			for name, block := range schema.BlockTypes {
				var labelNames []string
				if block.Nesting == configschema.NestingMap {
					labelNames = append(labelNames, "key")
				}
				bodySchema.Blocks = append(bodySchema.Blocks, hcl.BlockHeaderSchema{
					Type:       name,
					LabelNames: labelNames,
				})
			}
			// Verify that the file body matches the expected backend schema.
			_, schemaDiags := newBody.Content(&bodySchema)
			diags = diags.Append(schemaDiags)
			if schemaDiags.HasErrors() {
				continue
			}
			flushVals() // deal with any accumulated individual values first
			mergeBody(newBody)
		} else {
			name := item.Value[:eq]
			rawValue := item.Value[eq+1:]
			attrS := schema.Attributes[name]
			if attrS == nil {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Invalid backend configuration argument",
					fmt.Sprintf("The backend configuration argument %q given on the command line is not expected for the selected backend type.", name),
				))
				continue
			}
			value, valueDiags := configValueFromCLI(item.String(), rawValue, attrS.Type)
			diags = diags.Append(valueDiags)
			if valueDiags.HasErrors() {
				continue
			}
			synthVals[name] = value
		}
	}

	flushVals()

	return ret, diags
}

func (c *TfversionCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictDirs("")
}

func (c *TfversionCommand) AutocompleteFlags() complete.Flags {
	return complete.Flags{
		"-backend":        completePredictBoolean,
		"-cloud":          completePredictBoolean,
		"-backend-config": complete.PredictFiles("*.tfvars"), // can also be key=value, but we can't "predict" that
		"-force-copy":     complete.PredictNothing,
		"-from-module":    completePredictModuleSource,
		"-get":            completePredictBoolean,
		"-input":          completePredictBoolean,
		"-lock":           completePredictBoolean,
		"-lock-timeout":   complete.PredictAnything,
		"-no-color":       complete.PredictNothing,
		"-plugin-dir":     complete.PredictDirs(""),
		"-reconfigure":    complete.PredictNothing,
		"-migrate-state":  complete.PredictNothing,
		"-upgrade":        completePredictBoolean,
	}
}

func (c *TfversionCommand) Help() string {
	helpText := `
Usage: terraform [global options] init [options]

  Initialize a new or existing Terraform working directory by creating
  initial files, loading any remote state, downloading modules, etc.

  This is the first command that should be run for any new or existing
  Terraform configuration per machine. This sets up all the local data
  necessary to run Terraform that is typically not committed to version
  control.

  This command is always safe to run multiple times. Though subsequent runs
  may give errors, this command will never delete your configuration or
  state. Even so, if you have important information, please back it up prior
  to running this command, just in case.

Options:

  -backend=false          Disable backend or Terraform Cloud initialization
                          for this configuration and use what was previously
                          initialized instead.

                          aliases: -cloud=false

  -backend-config=path    Configuration to be merged with what is in the
                          configuration file's 'backend' block. This can be
                          either a path to an HCL file with key/value
                          assignments (same format as terraform.tfvars) or a
                          'key=value' format, and can be specified multiple
                          times. The backend type must be in the configuration
                          itself.

  -force-copy             Suppress prompts about copying state data when
                          initializating a new state backend. This is
                          equivalent to providing a "yes" to all confirmation
                          prompts.

  -from-module=SOURCE     Copy the contents of the given module into the target
                          directory before initialization.

  -get=false              Disable downloading modules for this configuration.

  -input=false            Disable interactive prompts. Note that some actions may
                          require interactive prompts and will error if input is
                          disabled.

  -lock=false             Don't hold a state lock during backend migration.
                          This is dangerous if others might concurrently run
                          commands against the same workspace.

  -lock-timeout=0s        Duration to retry a state lock.

  -no-color               If specified, output won't contain any color.

  -plugin-dir             Directory containing plugin binaries. This overrides all
                          default search paths for plugins, and prevents the
                          automatic installation of plugins. This flag can be used
                          multiple times.

  -reconfigure            Reconfigure a backend, ignoring any saved
                          configuration.

  -migrate-state          Reconfigure a backend, and attempt to migrate any
                          existing state.

  -upgrade                Install the latest module and provider versions
                          allowed within configured constraints, overriding the
                          default behavior of selecting exactly the version
                          recorded in the dependency lockfile.

  -lockfile=MODE          Set a dependency lockfile mode.
                          Currently only "readonly" is valid.

  -ignore-remote-version  A rare option used for Terraform Cloud and the remote backend
                          only. Set this to ignore checking that the local and remote
                          Terraform versions use compatible state representations, making
                          an operation proceed even when there is a potential mismatch.
                          See the documentation on configuring Terraform with
                          Terraform Cloud for more information.

`
	return strings.TrimSpace(helpText)
}

func (c *TfversionCommand) Synopsis() string {
	return "Prepare your working directory for other commands"
}

const errTfversionConfigError = `
[reset]There are some problems with the configuration, described below.

The Terraform configuration must be valid before initialization so that
Terraform can determine which modules and providers need to be installed.
`

const errTfversionCopyNotEmpty = `
The working directory already contains files. The -from-module option requires
an empty directory into which a copy of the referenced module will be placed.

To initialize the configuration already in this working directory, omit the
-from-module option.
`

const outputTfversionEmpty = `
[reset][bold]Terraform initialized in an empty directory![reset]

The directory has no Terraform configuration files. You may begin working
with Terraform immediately by creating Terraform configuration files.
`
