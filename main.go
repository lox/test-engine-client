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

type TestRunner interface {
	Command(testCases []string) (*exec.Cmd, error)
	GetExamples(files []string) ([]plan.TestCase, error)
	GetFiles() ([]string, error)
	RetryCommand() (*exec.Cmd, error)
}

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
	apiClient := api.NewClient(api.ClientConfig{
		ServerBaseUrl:    cfg.ServerBaseUrl,
		AccessToken:      cfg.AccessToken,
		OrganizationSlug: cfg.OrganizationSlug,
		Version:          cfg.Version,
	})

	testPlan, err := fetchOrCreateTestPlan(ctx, apiClient, cfg, files, testRunner)
	if err != nil {
		logErrorAndExit(16, "Couldn't fetch or create test plan: %v", err)
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

	var timeline []api.Timeline
	timeline = append(timeline, api.Timeline{
		Event:     "test_start",
		Timestamp: createTimestamp(),
	})
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

	err = cmd.Wait()
	timeline = append(timeline, api.Timeline{
		Event:     "test_end",
		Timestamp: createTimestamp(),
	})

	if err != nil {
		if exitError := new(exec.ExitError); errors.As(err, &exitError) {
			exitCode := exitError.ExitCode()
			if cfg.MaxRetries == 0 {
				// If retry is disabled, we exit immediately with the same exit code from the test runner
				sendMetadata(ctx, apiClient, cfg, timeline)
				logErrorAndExit(exitCode, "Rspec exited with error %v", err)
			} else {
				retryExitCode := retryFailedTests(testRunner, cfg.MaxRetries, &timeline)
				if retryExitCode != 0 {
					sendMetadata(ctx, apiClient, cfg, timeline)
					logErrorAndExit(retryExitCode, "Rspec exited with error %v after retry failing tests", err)
				}
			}
		} else {
			logErrorAndExit(16, "Couldn't run tests: %v", err)
		}
	}

	sendMetadata(ctx, apiClient, cfg, timeline)

	// Close the channel that will stop the goroutine.
	close(finishCh)
}

func createTimestamp() string {
	return time.Now().Format(time.RFC3339Nano)
}

func sendMetadata(ctx context.Context, apiClient *api.Client, cfg config.Config, timeline []api.Timeline) {
	err := apiClient.PostTestPlanMetadata(ctx, cfg.SuiteSlug, cfg.Identifier, api.TestPlanMetadataParams{
		Timeline:    timeline,
		SplitterEnv: cfg.DumpEnv(),
		Version:     Version,
	})

	if err != nil {
		fmt.Printf("Failed to send metadata: %v\n", err)
	}
}

func retryFailedTests(testRunner TestRunner, maxRetries int, timeline *[]api.Timeline) int {
	// Retry failed tests
	retries := 0
	for retries < maxRetries {
		retries++
		fmt.Printf("Attempt %d of %d to retry failing tests\n", retries, maxRetries)

		cmd, err := testRunner.RetryCommand()
		if err != nil {
			logErrorAndExit(16, "Couldn't process retry command: %v", err)
		}

		*timeline = append(*timeline, api.Timeline{
			Event:     fmt.Sprintf("retry_%d_start", retries),
			Timestamp: createTimestamp(),
		})
		if err := cmd.Start(); err != nil {
			logErrorAndExit(16, "Couldn't start tests: %v", err)
		}

		err = cmd.Wait()
		*timeline = append(*timeline, api.Timeline{
			Event:     fmt.Sprintf("retry_%d_end", retries),
			Timestamp: createTimestamp(),
		})
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
func fetchOrCreateTestPlan(ctx context.Context, apiClient *api.Client, cfg config.Config, files []string, testRunner TestRunner) (plan.TestPlan, error) {
	debug.Println("Fetching test plan")

	// Fetch the plan from the server's cache.
	cachedPlan, err := apiClient.FetchTestPlan(ctx, cfg.SuiteSlug, cfg.Identifier)

	handleError := func(err error) (plan.TestPlan, error) {
		if errors.Is(err, api.ErrRetryTimeout) {
			fmt.Println("Could not fetch or create plan from server, using fallback mode. Your build may take longer than usual.")
			p := plan.CreateFallbackPlan(files, cfg.Parallelism)
			return p, nil
		}
		return plan.TestPlan{}, err
	}

	if err != nil {
		return handleError(err)
	}

	if cachedPlan != nil {
		// The server can return an "error" plan indicated by an empty task list (i.e. `{"tasks": {}}`).
		// In this case, we should create a fallback plan.
		if len(cachedPlan.Tasks) == 0 {
			fmt.Println("Error plan received, using fallback mode. Your build may take longer than usual.")
			testPlan := plan.CreateFallbackPlan(files, cfg.Parallelism)
			return testPlan, nil
		}

		debug.Printf("Test plan found. Identifier: %q", cfg.Identifier)
		return *cachedPlan, nil
	}

	debug.Println("No test plan found, creating a new plan")
	// If the cache is empty, create a new plan.
	params, err := createRequestParam(ctx, cfg, files, *apiClient, testRunner)
	if err != nil {
		return handleError(err)
	}

	debug.Println("Creating test plan")
	testPlan, err := apiClient.CreateTestPlan(ctx, cfg.SuiteSlug, params)

	if err != nil {
		return handleError(err)
	}

	// The server can return an "error" plan indicated by an empty task list (i.e. `{"tasks": {}}`).
	// In this case, we should create a fallback plan.
	if len(testPlan.Tasks) == 0 {
		fmt.Println("Error plan received, using fallback mode. Your build may take longer than usual.")
		testPlan = plan.CreateFallbackPlan(files, cfg.Parallelism)
	}

	debug.Printf("Test plan created. Identifier: %q", cfg.Identifier)
	return testPlan, nil
}

type fileTiming struct {
	Path     string
	Duration time.Duration
}

// createRequestParam creates the request parameters for the test plan with the given configuration and files.
// The files should have been filtered by include/exclude patterns before passing to this function.
// If SplitByExample is disabled (default), it will return the default params that contain all the files.
// If SplitByExample is enabled, it will split the slow files into examples and return it along with the rest of the files.
//
// Error is returned if there is a failure to fetch test file timings or to get the test examples from test files when SplitByExample is enabled.
func createRequestParam(ctx context.Context, cfg config.Config, files []string, client api.Client, runner TestRunner) (api.TestPlanParams, error) {
	if !cfg.SplitByExample {
		debug.Println("Splitting by file")
		testCases := []plan.TestCase{}
		for _, file := range files {
			testCases = append(testCases, plan.TestCase{
				Path: file,
			})
		}

		return api.TestPlanParams{
			Mode:        cfg.Mode,
			Identifier:  cfg.Identifier,
			Parallelism: cfg.Parallelism,
			Tests: api.TestPlanParamsTest{
				Files: testCases,
			},
		}, nil
	}

	debug.Println("Splitting by example")

	debug.Printf("Fetching timings for %d files", len(files))
	// Fetch the timings for all files.
	timings, err := client.FetchFilesTiming(ctx, cfg.SuiteSlug, files)
	if err != nil {
		return api.TestPlanParams{}, fmt.Errorf("failed to fetch file timings: %w", err)
	}
	debug.Printf("Got timings for %d files", len(timings))

	// The server only returns timings for the files that has been run before.
	// Therefore, we need to merge the response with the requested files.
	// The files that are not in the response will have a duration of 0.
	allFilesTiming := []fileTiming{}
	for _, file := range files {
		allFilesTiming = append(allFilesTiming, fileTiming{
			Path:     file,
			Duration: timings[file],
		})
	}

	// Get files that has duration greater or equal to the slow file threshold.
	// Currently, the slow file threshold is set to 3 minutes which is roughly 70% of optimal 4 minutes node duration.
	slowFiles := []string{}
	restOfFiles := []plan.TestCase{}

	for _, timing := range allFilesTiming {
		if timing.Duration >= cfg.SlowFileThreshold {
			slowFiles = append(slowFiles, timing.Path)
		} else {
			restOfFiles = append(restOfFiles, plan.TestCase{
				Path: timing.Path,
			})
		}
	}

	if len(slowFiles) == 0 {
		debug.Println("No slow files found")
		return api.TestPlanParams{
			Mode:        cfg.Mode,
			Identifier:  cfg.Identifier,
			Parallelism: cfg.Parallelism,
			Tests: api.TestPlanParamsTest{
				Files: restOfFiles,
			},
		}, nil
	}

	debug.Printf("Getting examples for %d slow files", len(slowFiles))

	// Get the examples for the slow files.
	slowFilesExamples, err := runner.GetExamples(slowFiles)
	if err != nil {
		return api.TestPlanParams{}, fmt.Errorf("failed to get examples for slow files: %w", err)
	}

	debug.Printf("Got %d examples within the slow files", len(slowFilesExamples))

	return api.TestPlanParams{
		Mode:        cfg.Mode,
		Identifier:  cfg.Identifier,
		Parallelism: cfg.Parallelism,
		Tests: api.TestPlanParamsTest{
			Examples: slowFilesExamples,
			Files:    restOfFiles,
		},
	}, nil
}
