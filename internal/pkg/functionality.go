package pkg

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/capnfabs/grouse/internal/git"
	"github.com/capnfabs/grouse/internal/out"
	"github.com/kballard/go-shellquote"
	"github.com/pkg/errors"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
)

func check(err error) {
	if err != nil {
		panic(err)
	}
}

// TODO: get rid of this, put it into a dependency context or something.
var AppFs = afero.NewOsFs()

func RunRootCommand(cmd *cobra.Command) {
	context, err := parseArgs(cmd.Flags())
	if err != nil {
		out.Outln("Error:", err)
		cmd.Usage()
		os.Exit(1)
	}
	out.Debug = context.debug

	err = runMain(context)
	if err != nil {
		out.Outln("Error:", err)
		os.Exit(2)
	}
}

func runMain(context *cmdArgs) error {
	repo, err := git.OpenRepository(context.repoDir)

	if err != nil {
		return errors.WithMessagef(err, "Couldn't load the git repo in %s", context.repoDir)
	}

	relativeRoot, err := git.GetRelativeLocation(context.repoDir)
	// Shouldn't happen because we already verified the repo above?
	check(err)

	out.Debugf("Got repo location %#v and relative path %#v\n", repo.RootDir, relativeRoot)

	refs := []git.ResolvedUserRef{}

	for _, commit := range context.commits {
		ref, err := repo.ResolveCommit(commit)
		if err != nil {
			return errors.WithMessagef(err, "Couldn't resolve '%s' as git commit", commit)
		}
		refs = append(refs, ref)
	}

	out.Outf("Computing diff between revisions %s and %s\n", refs[0], refs[1])

	scratchDir, err := ioutil.TempDir("", "grouse-diff")
	// If this fails, we're unable to do anything with temp storage, so just
	// panic.
	check(err)

	srcWorktree, err := repo.AddWorktree(path.Join(scratchDir, "src"))
	check(err)
	if !context.keepWorktree {
		// Debug doesn't remove the worktree, so you can inspect it later.
		defer srcWorktree.Remove()
	}

	// Init the Output Repo
	outputDir := path.Join(scratchDir, "output")
	os.MkdirAll(outputDir, os.ModePerm)
	outputRepo, err := git.NewRepository(outputDir)
	// Not the user's fault and nothing we can do; panicking is ok.
	check(err)

	outputHashes := []git.Hash{}

	for _, ref := range refs {
		// Make sure the output directory is empty
		err = eraseDirectoryExceptRootDotGit(outputDir)
		check(err)

		out.Outf("Building revision %s…\n", ref)
		hash, err := processSourceAtCommit(
			srcWorktree, ref.Commit(), relativeRoot, context.buildArgs, outputRepo)

		switch err.(type) {
		case *exec.ExitError:
			err := errors.Wrapf(err, "Building at commit %s failed", ref)
			return err
		case error:
			panic(err)
		}
		outputHashes = append(outputHashes, hash)
	}

	// Do the actual diff
	out.Outln("Diffing…")
	err = runDiff(outputDir, context.diffCommand, context.diffArgs, outputHashes[0], outputHashes[1])
	switch e := err.(type) {
	case *exec.ExitError:
		if strings.Contains(e.Error(), "signal: broken pipe") {
			// It's not an error; but the user exited 'less' or whatever
		} else {
			err := errors.Wrapf(
				err, "Running git %s failed", context.diffCommand)
			return err
		}
	case error:
		panic(err)
	}
	return nil
}

func eraseDirectoryExceptRootDotGit(directory string) error {
	infos, err := afero.ReadDir(AppFs, directory)
	if err != nil {
		return err
	}
	for _, info := range infos {
		if info.Name() == ".git" {
			continue
		}

		err := AppFs.RemoveAll(path.Join(directory, info.Name()))
		if err != nil {
			return err
		}
	}
	return nil
}

func processSourceAtCommit(
	srcWorktree git.Worktree, ref git.ResolvedCommit, hugoRelativeRoot string, buildArgs []string, outputRepo git.Repository) (git.Hash, error) {
	out.Debugf("Checking out %s…\n", ref)
	err := srcWorktree.Checkout(ref)
	if err != nil {
		return git.NilHash, err
	}
	out.Debugln("…done checking out.")

	if err = runHugo(path.Join(srcWorktree.Location(), hugoRelativeRoot), outputRepo.RootDir(), buildArgs); err != nil {
		return git.NilHash, err
	}

	commitMessage := fmt.Sprintf("Website content, built from %s", ref)
	return outputRepo.CommitEverythingInWorktree(commitMessage)
}

func runHugo(hugoRootDir string, outputDir string, userArgs []string) error {
	// Put the 'destination' last. Repeated 'destination' flags only uses the
	// last one.
	// Note that we do it with the "--destination=/foo/" instead of "--destination foo"
	// -- there was a reason for this but it's been lost to time.
	allArgs := append(userArgs, "--destination="+shellquote.Join(outputDir))
	cmd := exec.Command("hugo", allArgs...)
	out.Debugf("Running command\n> %s\n(from directory %s)\n", shellquote.Join(cmd.Args...), hugoRootDir)
	cmd.Dir = hugoRootDir

	// TODO: if --debug is NOT specified, should hang on to these and then only
	// print them if an error occurs.
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	return cmd.Run()
}

func runDiff(repoDir, diffCommand string, userArgs []string, hash1, hash2 git.Hash) error {
	allArgs := []string{diffCommand}
	allArgs = append(allArgs, userArgs...)
	allArgs = append(allArgs, string(hash1), string(hash2))

	cmd := exec.Command("git", allArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Dir = repoDir
	out.Debugf("Running command %s\n", shellquote.Join(cmd.Args...))
	// This gets surfaced to the user because they're allowed to pass in diff
	// args, so it's probably (?) something they can fix?
	return cmd.Run()
}
