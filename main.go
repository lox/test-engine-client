// The test-splitter tool fetches and runs test plans generated by Buildkite
// Test Splitting.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"time"

	"github.com/buildkite/test-splitter/internal/api"
	"github.com/buildkite/test-splitter/internal/config"
	"github.com/buildkite/test-splitter/internal/debug"
	"github.com/buildkite/test-splitter/internal/plan"
	"github.com/buildkite/test-splitter/internal/runner"
)

var Version = ""

func main() {
	debug.SetDebug(os.Getenv("BUILDKITE_SPLITTER_DEBUG_ENABLED") == "true")

	// get config
	cfg, err := config.New()
	if err != nil {
		logErrorAndExit(16, "Invalid configuration: %v", err)
	}

	// TODO: detect test runner and use appropriate runner
	testRunner := runner.NewRspec(cfg.TestCommand)

	versionFlag := flag.Bool("version", false, "print version information")

	// Gathering files
	flag.Parse()

	if *versionFlag {
		fmt.Println(Version)
		os.Exit(0)
	}

	files, err := testRunner.GetFiles()
	if err != nil {
		logErrorAndExit(16, "Couldn't get files: %v", err)
	}

	// get plan
	ctx := context.Background()
	// We expect the whole test plan fetching process takes no more than 60 seconds.
	// Configure the timeout as 70s to give it a bit more buffer.
	fetchCtx, cancel := context.WithTimeout(ctx, 70*time.Second)
	defer cancel()

	testPlan, err := fetchOrCreateTestPlan(fetchCtx, cfg, files)
	if err != nil {
		logErrorAndExit(16, "Couldn't create test plan: %v", err)
	}

	// get plan for this node
	thisNodeTask := testPlan.Tasks[strconv.Itoa(cfg.NodeIndex)]

	// execute tests
	runnableTests := []string{}
	for _, testCase := range thisNodeTask.Tests {
		runnableTests = append(runnableTests, testCase.Path)
	}

	cmd, err := testRunner.Command(runnableTests)
	if err != nil {
		logErrorAndExit(16, "Couldn't process test command: %q, %v", testRunner.TestCommand, err)
	}

	if err := cmd.Start(); err != nil {
		logErrorAndExit(16, "Couldn't start tests: %v", err)
	}

	// Create a channel that will be closed when the command finishes.
	finishCh := make(chan struct{})

	// Start a goroutine to that waits for a signal or the command to finish.
	go func() {
		// Create another channel to receive the signals.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh)

		// Wait for a signal to be received or the command to finish.
		// Because a message can come through both channels asynchronously,
		// we use for loop to listen to both channels and select the one that has a message.
		// Without for loop, only one case would be selected and the other would be ignored.
		// If the signal is received first, the finishCh will never get processed and the goroutine will run forever.
		for {
			select {
			case sig := <-sigCh:
				// When a signal is received, forward it to the command.
				cmd.Process.Signal(sig)
			case <-finishCh:
				// When the the command finishes, we stop listening for signals and return.
				signal.Stop(sigCh)
				return
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		if exitError := new(exec.ExitError); errors.As(err, &exitError) {
			exitCode := exitError.ExitCode()
			if cfg.MaxRetries == 0 {
				// If retry is disabled, we exit immediately with the same exit code from the test runner
				logErrorAndExit(exitCode, "Rspec exited with error %v", err)
			} else {
				retryExitCode := retryFailedTests(testRunner, cfg.MaxRetries)
				if retryExitCode == 0 {
					os.Exit(0)
				} else {
					logErrorAndExit(retryExitCode, "Rspec exited with error %v after retry failing tests", err)
				}
			}
		}
		logErrorAndExit(16, "Couldn't run tests: %v", err)
	}

	// Close the channel that will stop the goroutine.
	close(finishCh)
}

func retryFailedTests(testRunner runner.Rspec, maxRetries int) int {
	// Retry failed tests
	retries := 0
	for retries < maxRetries {
		retries++
		fmt.Printf("Attempt %d of %d to retry failing tests\n", retries, maxRetries)

		cmd, err := testRunner.RetryCommand()
		if err != nil {
			logErrorAndExit(16, "Couldn't process retry command: %v", err)
		}

		if err := cmd.Start(); err != nil {
			logErrorAndExit(16, "Couldn't start tests: %v", err)
		}

		err = cmd.Wait()
		if err != nil {
			if exitError := new(exec.ExitError); errors.As(err, &exitError) {
				exitCode := exitError.ExitCode()
				if retries >= maxRetries {
					// If the command exits with an error and we've reached the maximum number of retries, we exit.
					return exitCode
				}
			}
		} else {
			// If the failing tests pass after retry (test command exits without error), we exit with code 0.
			return 0
		}
	}
	return 1
}

// logErrorAndExit logs an error message and exits with the given exit code.
func logErrorAndExit(exitCode int, format string, v ...any) {
	fmt.Printf(format+"\n", v...)
	os.Exit(exitCode)
}

// fetchOrCreateTestPlan fetches a test plan from the server, or creates a
// fallback plan if the server is unavailable or returns an error plan.
func fetchOrCreateTestPlan(ctx context.Context, cfg config.Config, files []string) (plan.TestPlan, error) {
	apiClient := api.NewClient(api.ClientConfig{
		ServerBaseUrl:    cfg.ServerBaseUrl,
		AccessToken:      cfg.AccessToken,
		OrganizationSlug: cfg.OrganizationSlug,
	})

	// Fetch the plan from the server's cache.
	cachedPlan, err := apiClient.FetchTestPlan(cfg.SuiteSlug, cfg.Identifier)

	if err != nil {
		return plan.TestPlan{}, err
	}

	if cachedPlan != nil {
		return *cachedPlan, nil
	}

	// If the cache is empty, create a new plan.
	testCases := []plan.TestCase{}
	for _, file := range files {
		testCases = append(testCases, plan.TestCase{
			Path: file,
		})
	}

	testPlan, err := apiClient.CreateTestPlan(ctx, cfg.SuiteSlug, api.TestPlanParams{
		Mode:        cfg.Mode,
		Identifier:  cfg.Identifier,
		Parallelism: cfg.Parallelism,
		Tests: api.TestPlanParamsTest{
			Files: testCases,
		},
	})

	if err != nil {
		// Didn't exceed context deadline? Must have been some kind of error that
		// means we should return error to main function and abort.
		if !errors.Is(err, context.DeadlineExceeded) {
			return plan.TestPlan{}, err
		}
		// Create the fallback plan
		fmt.Println("Could not fetch plan from server, using fallback mode. Your build may take longer than usual.")
		testPlan = plan.CreateFallbackPlan(testCases, cfg.Parallelism)
	}

	// The server can return an "error" plan indicated by an empty task list (i.e. `{"tasks": {}}`).
	// In this case, we should create a fallback plan.
	if len(testPlan.Tasks) == 0 {
		fmt.Println("Test splitter server returned an error, using fallback mode. Your build may take longer than usual.")
		testPlan = plan.CreateFallbackPlan(testCases, cfg.Parallelism)
	}

	return testPlan, nil
}
