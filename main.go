// The test-splitter tool fetches and runs test plans generated by Buildkite
// Test Splitting.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/buildkite/test-splitter/internal/api"
	"github.com/buildkite/test-splitter/internal/plan"
	"github.com/buildkite/test-splitter/internal/runner"
)

// other attributes are omitted for simplicity
type Example struct {
	FilePath string `json:"file_path"`
}

// other attributes are omitted for simplicity
type RspecData struct {
	Examples []Example `json:"examples"`
}

func main() {
	// TODO: detect test runner and use appropriate runner
	testRunner := runner.Rspec{}

	// get files
	fmt.Println("--- :test-analytics: Gathering test plan context and creating test plan request 🐿️")
	files, err := testRunner.GetFiles()
	if err != nil {
		log.Fatalf("Couldn't get files: %v", err)
	}
	fmt.Printf("Found %d files\n", len(files))

	// fetch env vars
	suiteToken := FetchEnv("BUILDKITE_SPLITTER_RSPEC_TOKEN", "xx-local-analytics-key")
	identifier := FetchEnv("BUILDKITE_BUILD_ID", "local")
	mode := FetchEnv("BUILDKITE_SPLITTER_MODE", "static")
	parralelism, err := FetchIntEnv("BUILDKITE_PARALLEL_JOB_COUNT", 1)
	if err != nil {
		log.Fatalf("Misconfigured parallel job count: %v", err)
	}
	splitterPath := FetchEnv("BUILDKITE_SPLITTER_PATH", "https://buildkite.com")

	// get plan
	fmt.Println("--- :test-analytics: Getting Test Plan 🎣")
	fmt.Println("SuiteToken: ", suiteToken)
	fmt.Println("Mode: ", mode)
	fmt.Println("Identifier: ", identifier)
	fmt.Println("Parallelism: ", parralelism)

	testCases := []plan.TestCase{}
	for _, file := range files {
		testCases = append(testCases, plan.TestCase{
			Path: file,
		})
	}

	plan, err := api.FetchTestPlan(splitterPath, api.TestPlanParams{
		SuiteToken:  suiteToken,
		Mode:        mode,
		Identifier:  identifier,
		Parallelism: parralelism,
		Tests: plan.Tests{
			Cases:  testCases,
			Format: "files",
		},
	})
	if err != nil {
		log.Fatalf("Couldn't fetch test plan: %v", err)
	}

	// get plan for this node
	nodeIdx := FetchEnv("BUILDKITE_PARALLEL_JOB", "0")
	thisNodeTask := plan.Tasks[nodeIdx]

	prettifiedPlan, _ := json.MarshalIndent(thisNodeTask, "", "  ")
	fmt.Println("--- :test-analytics: Plan for this node 🎊")
	fmt.Println(string(prettifiedPlan))

	// execute tests
	runnableTests := []string{}
	for _, testCase := range thisNodeTask.Tests.Cases {
		runnableTests = append(runnableTests, testCase.Path)
	}
	err = testRunner.Run(runnableTests)

	if err != nil {
		// TODO: bubble up rspec error to main process
		log.Fatal("Error when executing tests: ", err)
	}

	fmt.Println("--- :test-analytics: Test execution results 📊")
	err = testRunner.Report(os.Stdout, thisNodeTask.Tests.Cases)
	if err != nil {
		fmt.Println(err)
	}
}