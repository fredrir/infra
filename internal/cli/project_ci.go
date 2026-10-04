package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/pipeline"
	"github.com/fredrir/infra/internal/process"
	"github.com/spf13/cobra"
)

func newProjectCICommand() *cobra.Command {
	var infraRoot, source, repository, repositoryID string
	project := &cobra.Command{Use: "project", Short: "Execute centrally configured project CI", RunE: missingCommand}
	project.PersistentFlags().StringVar(&infraRoot, "infra-root", ".", "Infrastructure checkout")
	project.PersistentFlags().StringVar(&source, "root", ".", "Source checkout")
	project.PersistentFlags().StringVar(&repository, "repository", os.Getenv("GITHUB_REPOSITORY"), "Source repository")
	project.PersistentFlags().StringVar(&repositoryID, "repository-id", os.Getenv("GITHUB_REPOSITORY_ID"), "Source repository ID")
	var base, output, purpose string
	var scheduled bool
	plan := &cobra.Command{Use: "plan", Short: "Select shared project checks and images", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		profile, err := ci.ReadProject(infraRoot, repository, repositoryID)
		if err != nil {
			return err
		}
		var variables map[string]string
		switch purpose {
		case "checks", "image-checks":
			profile.Images = nil
			if purpose == "image-checks" {
				checks := profile.Checks[:0]
				for _, check := range profile.Checks {
					if !check.InImage {
						checks = append(checks, check)
					}
				}
				profile.Checks = checks
			}
		case "images":
			variables = map[string]string{}
			for _, image := range profile.Images {
				for _, name := range image.Variables {
					variables[name] = os.Getenv(name)
				}
			}
			if profile.Doppler != nil {
				values, err := ci.PublicProjectVariables(command.Context(), &http.Client{Timeout: 30 * time.Second}, "https://api.doppler.com", os.Getenv("DOPPLER_TOKEN"), *profile.Doppler)
				if err != nil {
					return err
				}
				for name, value := range values {
					variables[name] = value
				}
			}
			profile.Checks = nil
		default:
			return errors.New("purpose must be checks, image-checks or images")
		}
		result, err := ci.PlanProject(command.Context(), process.Runner{Dir: source, Stderr: command.ErrOrStderr()}, profile, base, scheduled, variables)
		if err != nil {
			return err
		}
		if err := ci.WriteProjectOutputs(output, result); err != nil {
			return err
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(result)
	}}
	plan.Flags().StringVar(&base, "base", os.Getenv("BASE"), "Previous source revision")
	plan.Flags().StringVar(&output, "github-output", os.Getenv("GITHUB_OUTPUT"), "Workflow output file")
	plan.Flags().StringVar(&purpose, "purpose", "checks", "Select checks or images")
	plan.Flags().BoolVar(&scheduled, "scheduled", os.Getenv("GITHUB_EVENT_NAME") == "schedule", "Include expensive scheduled checks")
	var name, suite, report string
	var capture bool
	check := &cobra.Command{Use: "check", Short: "Run a project check with isolated services", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		err := ci.ProjectCheckCommands(command.Context(), process.Runner{Dir: source, Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}, name, suite, report)
		if capture && err != nil {
			if report == "" {
				return err
			}
			if writeErr := os.MkdirAll(report, 0755); writeErr != nil {
				return errors.Join(err, writeErr)
			}
			code, readErr := os.ReadFile(filepath.Join(report, "exit-code"))
			if readErr != nil || strings.TrimSpace(string(code)) == "0" {
				if writeErr := os.WriteFile(filepath.Join(report, "exit-code"), []byte("1\n"), 0600); writeErr != nil {
					return errors.Join(err, writeErr)
				}
			}
			fmt.Fprintln(command.ErrOrStderr(), err)
			return nil
		}
		return err
	}}
	check.Flags().StringVar(&name, "project", "", "Project name")
	check.MarkFlagRequired("project")
	check.Flags().StringVar(&suite, "suite", "", "Check suite")
	check.MarkFlagRequired("suite")
	check.Flags().StringVar(&report, "report-dir", "/reports", "Check reports")
	check.Flags().BoolVar(&capture, "capture-result", false, "Export failure reports for the host to enforce")
	var binary, runSuite, runReport string
	run := &cobra.Command{Use: "run", Short: "Run a shared project check through Dagger", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		profile, err := ci.ReadProject(infraRoot, repository, repositoryID)
		if err != nil {
			return err
		}
		for _, selected := range profile.Checks {
			if selected.Name != runSuite {
				continue
			}
			infraRoot, err = filepath.Abs(infraRoot)
			if err != nil {
				return err
			}
			source, err = filepath.Abs(source)
			if err != nil {
				return err
			}
			sourceRevision, err := (process.Runner{Dir: source}).Output(command.Context(), "git", "rev-parse", "HEAD")
			if err != nil {
				return err
			}
			arguments := map[string]string{"CI_REVISION": strings.TrimSpace(string(sourceRevision))}
			for name, value := range selected.Arguments {
				arguments[name] = value
			}
			opts := pipeline.ImageOptions{Root: infraRoot, Context: source, Dockerfile: filepath.Join(infraRoot, selected.Recipe), Target: selected.Target, CheckOnly: true, InfraBinary: binary, Platform: "linux/amd64", BuildArgs: arguments, Log: command.ErrOrStderr()}
			if selected.Name == "mutation" || strings.HasPrefix(selected.Name, "fuzz-") {
				opts.CheckOnly = false
				opts.ExportDirectory = runReport
			}
			ctx, cancel := context.WithTimeout(command.Context(), time.Duration(selected.Timeout)*time.Minute)
			defer cancel()
			if _, err := pipeline.Image(ctx, opts); err != nil {
				return err
			}
			if opts.ExportDirectory != "" {
				code, err := os.ReadFile(filepath.Join(runReport, "exit-code"))
				if err != nil {
					return err
				}
				if strings.TrimSpace(string(code)) != "0" {
					return fmt.Errorf("%s failed; reports: %s", selected.Name, runReport)
				}
			}
			return nil
		}
		return fmt.Errorf("unknown project suite %s", runSuite)
	}}
	run.Flags().StringVar(&binary, "infra-binary", os.Getenv("INFRA_BINARY"), "Linux amd64 infrastructure binary")
	run.MarkFlagRequired("infra-binary")
	run.Flags().StringVar(&runSuite, "suite", "", "Check suite")
	run.MarkFlagRequired("suite")
	run.Flags().StringVar(&runReport, "report-dir", ".infra-reports", "Exported check reports")
	project.AddCommand(plan, check, run)
	return project
}

func newDependenciesCommand() *cobra.Command {
	var root, infraRoot, output, image, registryBase string
	command := &cobra.Command{Use: "dependencies plan|lookup|verify|resolve", Short: "Resolve verified runtime dependency images", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		if !strings.Contains("|plan|lookup|verify|resolve|", "|"+args[0]+"|") {
			return errors.New("unknown dependency operation")
		}
		runner := process.Runner{Dir: root, Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}
		plan, err := ci.PlanDependencies(command.Context(), runner, infraRoot)
		if err != nil {
			return err
		}
		registry := ci.DependencyRegistry{Client: &http.Client{Timeout: 15 * time.Second}, Base: registryBase, Actor: environmentValue("GITHUB_ACTOR", "fredrir"), Token: os.Getenv("REGISTRY_TOKEN")}
		if args[0] == "lookup" || args[0] == "resolve" {
			plan, err = registry.Lookup(command.Context(), plan)
			if err != nil {
				return err
			}
		}
		if args[0] == "verify" {
			plan.Image = image
		}
		if (args[0] == "lookup" && plan.Image != "") || args[0] == "verify" || args[0] == "resolve" {
			if plan.Image == "" {
				return fmt.Errorf("dependency image missing: %s:%s", plan.Repository, plan.Tag)
			}
			if err := registry.Verify(command.Context(), runner, infraRoot, plan, plan.Image); err != nil {
				return err
			}
			plan.Build = false
		}
		if err := ci.WriteOutputs(output, map[string]string{"key": plan.Key, "tag": plan.Tag, "repository": plan.Repository, "dockerfile": plan.Dockerfile, "build-args": plan.BuildArgs, "image": plan.Image, "build": fmt.Sprint(plan.Build)}); err != nil {
			return err
		}
		if args[0] == "resolve" {
			_, err = fmt.Fprintln(command.OutOrStdout(), plan.Image)
			return err
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(plan)
	}}
	command.Flags().StringVar(&root, "root", ".", "Source checkout")
	command.Flags().StringVar(&infraRoot, "infra-root", ".", "Infrastructure checkout")
	command.Flags().StringVar(&output, "github-output", os.Getenv("GITHUB_OUTPUT"), "Workflow output file")
	command.Flags().StringVar(&image, "image", os.Getenv("IMAGE"), "Immutable dependency image")
	command.Flags().StringVar(&registryBase, "registry", "https://ghcr.io", "Registry URL")
	return command
}
