package command

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/hashicorp/atlas-go/v1"
	"github.com/hashicorp/atlas-go/archive"
	"github.com/mitchellh/packer/packer"
)

// archiveTemplateEntry is the name the template always takes within the slug.
const archiveTemplateEntry = ".packer-template"

type PushCommand struct {
	Meta

	client *atlas.Client

	// For tests:
	uploadFn func(io.Reader, *uploadOpts) (<-chan struct{}, <-chan error, error)
}

func (c *PushCommand) Run(args []string) int {
	var create bool
	var token string

	f := flag.NewFlagSet("push", flag.ContinueOnError)
	f.Usage = func() { c.Ui.Error(c.Help()) }
	f.BoolVar(&create, "create", false, "create")
	f.StringVar(&token, "token", "", "token")
	if err := f.Parse(args); err != nil {
		return 1
	}

	args = f.Args()
	if len(args) != 1 {
		f.Usage()
		return 1
	}

	// Read the template
	tpl, err := packer.ParseTemplateFile(args[0], nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to parse template: %s", err))
		return 1
	}

	// Validate some things
	if tpl.Push.Name == "" {
		c.Ui.Error(fmt.Sprintf(
			"The 'push' section must be specified in the template with\n" +
				"at least the 'name' option set."))
		return 1
	}

	// Determine our token
	if token == "" {
		token = tpl.Push.Token
	}

	// Build our client
	defer func() { c.client = nil }()
	c.client = atlas.DefaultClient()
	if tpl.Push.Address != "" {
		c.client, err = atlas.NewClient(tpl.Push.Address)
		if err != nil {
			c.Ui.Error(fmt.Sprintf(
				"Error setting up API client: %s", err))
			return 1
		}
	}
	c.client.Token = token

	// Build the archiving options
	var opts archive.ArchiveOpts
	opts.Include = tpl.Push.Include
	opts.Exclude = tpl.Push.Exclude
	opts.VCS = tpl.Push.VCS
	opts.Extra = map[string]string{
		archiveTemplateEntry: args[0],
	}

	// Determine the path we're archiving
	path := tpl.Push.BaseDir
	if path == "" {
		path, err = filepath.Abs(args[0])
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error determining path to archive: %s", err))
			return 1
		}
		path = filepath.Dir(path)
	}

	// Build the upload options
	var uploadOpts uploadOpts
	uploadOpts.Slug = tpl.Push.Name
	uploadOpts.Builds = make(map[string]string)
	for _, b := range tpl.Builders {
		uploadOpts.Builds[b.Name] = b.Type
	}

	// Create the build config if it doesn't currently exist.
	if err := c.create(uploadOpts.Slug, create); err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	// Start the archiving process
	r, err := archive.CreateArchive(path, &opts)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error archiving: %s", err))
		return 1
	}
	defer r.Close()

	// Start the upload process
	doneCh, uploadErrCh, err := c.upload(r, &uploadOpts)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error starting upload: %s", err))
		return 1
	}

	err = nil
	select {
	case err = <-uploadErrCh:
		err = fmt.Errorf("Error uploading: %s", err)
	case <-doneCh:
	}

	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	c.Ui.Output(fmt.Sprintf("Push successful to '%s'", tpl.Push.Name))
	return 0
}

func (*PushCommand) Help() string {
	helpText := `
Usage: packer push [options] TEMPLATE

  Push the template and the files it needs to a Packer build service.
  This will not initiate any builds, it will only update the templates
  used for builds.

  The configuration about what is pushed is configured within the
  template's "push" section.

Options:

  -create             Create the build configuration if it doesn't exist.

  -token=<token>      Access token to use to upload. If blank, the
                      TODO environmental variable will be used.
`

	return strings.TrimSpace(helpText)
}

func (*PushCommand) Synopsis() string {
	return "push template files to a Packer build service"
}

func (c *PushCommand) create(name string, create bool) error {
	if c.uploadFn != nil {
		return nil
	}

	// Separate the slug into the user and name components
	user, name, err := atlas.ParseSlug(name)
	if err != nil {
		return fmt.Errorf("Malformed push name: %s", err)
	}

	// Check if it exists. If so, we're done.
	if _, err := c.client.BuildConfig(user, name); err == nil {
		return nil
	} else if err != atlas.ErrNotFound {
		return err
	}

	// Otherwise, show an error if we're not creating.
	if !create {
		return fmt.Errorf(
			"Push target doesn't exist: %s. Either create this online via\n" +
				"the website or pass the -create flag.", name)
	}

	// Create it
	if err := c.client.CreateBuildConfig(user, name); err != nil {
		return err
	}

	return nil
}

func (c *PushCommand) upload(
	r *archive.Archive, opts *uploadOpts) (<-chan struct{}, <-chan error, error) {
	if c.uploadFn != nil {
		return c.uploadFn(r, opts)
	}

	// Separate the slug into the user and name components
	user, name, err := atlas.ParseSlug(opts.Slug)
	if err != nil {
		return nil, nil, fmt.Errorf("upload: %s", err)
	}

	// Get the app
	bc, err := c.client.BuildConfig(user, name)
	if err != nil {
		return nil, nil, fmt.Errorf("upload: %s", err)
	}

	// Build the version to send up
	version := atlas.BuildConfigVersion{
		User:   bc.User,
		Name:   bc.Name,
		Builds: make([]atlas.BuildConfigBuild, 0, len(opts.Builds)),
	}
	for name, t := range opts.Builds {
		version.Builds = append(version.Builds, atlas.BuildConfigBuild{
			Name: name,
			Type: t,
		})
	}

	// Start the upload
	doneCh, errCh := make(chan struct{}), make(chan error)
	go func() {
		err := c.client.UploadBuildConfigVersion(&version, r, r.Size)
		if err != nil {
			errCh <- err
			return
		}

		close(doneCh)
	}()

	return doneCh, errCh, nil
}

type uploadOpts struct {
	URL    string
	Slug   string
	Builds map[string]string
}
