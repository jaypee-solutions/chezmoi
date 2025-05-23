package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/twpayne/chezmoi/v2/internal/chezmoi"
	"github.com/twpayne/chezmoi/v2/internal/chezmoierrors"
)

type mergeCmdConfig struct {
	Command string   `json:"command" mapstructure:"command" yaml:"command"`
	Args    []string `json:"args"    mapstructure:"args"    yaml:"args"`
}

func (c *Config) newMergeCmd() *cobra.Command {
	mergeCmd := &cobra.Command{
		Use:               "merge target...",
		Args:              cobra.MinimumNArgs(1),
		Short:             "Perform a three-way merge between the destination state, the source state, and the target state",
		Long:              mustLongHelp("merge"),
		Example:           example("merge"),
		ValidArgsFunction: c.targetValidArgs,
		RunE:              c.makeRunEWithSourceState(c.runMergeCmd),
		Annotations: newAnnotations(
			modifiesSourceDirectory,
			requiresSourceDirectory,
			persistentStateModeReadWrite,
		),
	}

	return mergeCmd
}

func (c *Config) runMergeCmd(cmd *cobra.Command, args []string, sourceState *chezmoi.SourceState) error {
	targetRelPaths, err := c.targetRelPaths(sourceState, args, targetRelPathsOptions{
		mustBeInSourceState: true,
		recursive:           true,
	})
	if err != nil {
		return err
	}

	for _, targetRelPath := range targetRelPaths {
		sourceStateEntry := sourceState.MustEntry(targetRelPath)
		if err := c.doMerge(targetRelPath, sourceStateEntry); err != nil {
			return err
		}
	}

	return nil
}

// doMerge is the core merge functionality. It invokes the merge tool to do a
// three-way merge between the destination, source, and target, including
// transparently decrypting the file in the source state.
func (c *Config) doMerge(targetRelPath chezmoi.RelPath, sourceStateEntry chezmoi.SourceStateEntry) (err error) {
	sourceAbsPath := c.SourceDirAbsPath.Join(sourceStateEntry.SourceRelPath().RelPath())

	// If the source state entry is an encrypted file, then decrypt it to a
	// temporary directory and pass the plaintext to the merge command
	// instead.
	var plaintextAbsPath chezmoi.AbsPath
	if sourceStateFile, ok := sourceStateEntry.(*chezmoi.SourceStateFile); ok {
		if sourceStateFile.Attr.Encrypted {
			var plaintextTempDirAbsPath chezmoi.AbsPath
			if plaintextTempDirAbsPath, err = c.tempDir("chezmoi-merge-plaintext"); err != nil {
				return err
			}
			plaintextAbsPath = plaintextTempDirAbsPath.Join(sourceStateEntry.SourceRelPath().RelPath())
			defer chezmoierrors.CombineFunc(&err, func() error {
				return os.RemoveAll(plaintextTempDirAbsPath.String())
			})
			var plaintext []byte
			if plaintext, err = sourceStateFile.Contents(); err != nil {
				return err
			}
			if err := chezmoi.MkdirAll(c.baseSystem, plaintextAbsPath.Dir(), 0o700); err != nil {
				return err
			}
			if err := c.baseSystem.WriteFile(plaintextAbsPath, plaintext, 0o600); err != nil {
				return err
			}
			sourceAbsPath = plaintextAbsPath
		}
	}

	// FIXME sourceStateEntry.TargetStateEntry eagerly evaluates the return
	// targetStateEntry's contents, which means that we cannot fallback to a
	// two-way merge if the source state's contents cannot be decrypted or
	// are an invalid template
	var targetStateEntry chezmoi.TargetStateEntry
	if targetStateEntry, err = sourceStateEntry.TargetStateEntry(c.destSystem, c.DestDirAbsPath.Join(targetRelPath)); err != nil {
		return fmt.Errorf("%s: %w", targetRelPath, err)
	}
	targetStateFile, ok := targetStateEntry.(*chezmoi.TargetStateFile)
	if !ok {
		// LATER consider handling symlinks?
		return fmt.Errorf("%s: not a file", targetRelPath)
	}
	var contents []byte
	if contents, err = targetStateFile.Contents(); err != nil {
		return err
	}

	// Create a temporary directory to store the target state and ensure that it
	// is removed afterwards.
	var tempDirAbsPath chezmoi.AbsPath
	if tempDirAbsPath, err = c.tempDir("chezmoi-merge"); err != nil {
		return err
	}

	targetStateAbsPath := tempDirAbsPath.JoinString(targetRelPath.Base())
	if err := c.baseSystem.WriteFile(targetStateAbsPath, contents, 0o600); err != nil {
		return err
	}

	templateData := struct {
		Destination string
		Source      string
		Target      string
	}{
		Destination: c.DestDirAbsPath.Join(targetRelPath).String(),
		Source:      sourceAbsPath.String(),
		Target:      targetStateAbsPath.String(),
	}

	args := make([]string, 0, len(c.Merge.Args))

	// Work around a regression introduced in 2.1.4
	// (https://github.com/twpayne/chezmoi/pull/1324) in a user-friendly
	// way.
	//
	// Prior to #1324, the merge.args config option was prepended to the
	// default order of files to the merge command. Post #1324, the
	// merge.args config option replaced all arguments to the merge command.
	//
	// Work around this by looking for any templates in merge.args. An arg
	// is considered a template if, after execution as a template, it is
	// not equal to the original arg.
	anyTemplateArgs := false
	for i, arg := range c.Merge.Args {
		var tmpl *template.Template
		if tmpl, err = template.New("merge.args[" + strconv.Itoa(i) + "]").Parse(arg); err != nil {
			return err
		}

		builder := strings.Builder{}
		if err := tmpl.Execute(&builder, templateData); err != nil {
			return err
		}
		args = append(args, builder.String())

		// Detect template arguments.
		if arg != builder.String() {
			anyTemplateArgs = true
		}
	}

	// If there are no template arguments, then append the destination,
	// source, and target paths as prior to #1324.
	if !anyTemplateArgs {
		args = append(args, templateData.Destination, templateData.Source, templateData.Target)
	}

	if err := c.persistentState.Close(); err != nil {
		return err
	}

	if err = c.run(c.DestDirAbsPath, c.Merge.Command, args); err != nil {
		return fmt.Errorf("%s: %w", targetRelPath, err)
	}

	// If the source state entry was an encrypted file, then re-encrypt the
	// plaintext.
	if !plaintextAbsPath.IsEmpty() {
		var encryptedContents []byte
		if encryptedContents, err = c.encryption.EncryptFile(plaintextAbsPath); err != nil {
			return err
		}
		if err := c.baseSystem.WriteFile(c.SourceDirAbsPath.Join(sourceStateEntry.SourceRelPath().RelPath()), encryptedContents, 0o644); err != nil {
			return err
		}
	}

	return nil
}
