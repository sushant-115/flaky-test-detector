package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// TestResult represents the outcome of a single test run.
type TestResult struct {
	Name      string    // Name of the test (e.g., TestSomething)
	Status    string    // "PASS", "FAIL", "SKIP"
	Duration  float64   // Duration in seconds
	Timestamp time.Time // When the test result was recorded
	Package   string    // Go package the test belongs to
}

// TestHistory stores all recorded results for a specific test.
type TestHistory struct {
	Results []TestResult
}

// FlakyTest represents a test identified as flaky, with its flakiness score.
type FlakyTest struct {
	Name           string
	Package        string
	FlakinessScore float64 // e.g., failure rate
	TotalRuns      int
	Failures       int
}

// parseGoTestOutput parses the output from `go test -v` and returns a slice of TestResult.
// It handles both PASS and FAIL lines, extracting test name, package, status, and duration.
func parseGoTestOutput(reader io.Reader) ([]TestResult, error) {
	scanner := bufio.NewScanner(reader)
	var results []TestResult

	// Regex to capture test results:
	// Example lines:
	// --- PASS: TestSomething (0.01s)
	// --- FAIL: TestAnother (0.00s)
	// ok      github.com/user/repo/pkg    0.005s
	// FAIL    github.com/user/repo/pkg    0.005s
	// The regex is designed to capture the test name, status, and duration from "--- PASS/FAIL" lines.
	// And then the package and overall status/duration from "ok" or "FAIL" lines.
	// We'll use the "ok/FAIL" lines to infer the package for the preceding individual tests.
	testLineRegex := regexp.MustCompile(`^--- (PASS|FAIL|SKIP): (.+) \(([\d.]+)s\)$`)
	packageLineRegex := regexp.MustCompile(`^(ok|FAIL|SKIP)\s+(\S+)\s+([\d.]+)s(?:\s+\[build failed\])?$`)

	currentPackage := ""
	for scanner.Scan() {
		line := scanner.Text()
		now := time.Now() // Use current time for timestamp

		// Try to match individual test result lines
		if matches := testLineRegex.FindStringSubmatch(line); len(matches) == 4 {
			status := matches[1]
			testName := matches[2]
			durationStr := matches[3]
			duration, err := strconv.ParseFloat(durationStr, 64)
			if err != nil {
				log.Printf("Warning: Could not parse duration '%s' for test '%s': %v", durationStr, testName, err)
				continue
			}

			results = append(results, TestResult{
				Name:      testName,
				Status:    status,
				Duration:  duration,
				Timestamp: now,
				Package:   currentPackage, // Assign the last known package
			})
		} else if matches := packageLineRegex.FindStringSubmatch(line); len(matches) >= 3 {
			// This line indicates the status of a whole package
			currentPackage = matches[2] // Update current package context
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading input: %w", err)
	}

	// After parsing, go back and assign packages to tests that might have been missed
	// due to package line appearing after the first test result in a package.
	// This is a simplistic approach; a more robust parser might build a tree.
	for i := len(results) - 1; i >= 0; i-- {
		if results[i].Package == "" {
			// Look for the nearest package line before this test
			for j := i - 1; j >= 0; j-- {
				if results[j].Package != "" {
					results[i].Package = results[j].Package
					break
				}
			}
		}
	}

	return results, nil
}

// calculateFlakiness analyzes test results and identifies flaky tests.
// For simplicity, a test is considered "flaky" if its failure rate exceeds a threshold.
// In a real tool, this would involve more complex heuristics (e.g., pass-fail-pass patterns, retries).
func calculateFlakiness(allResults []TestResult, threshold float64) []FlakyTest {
	testAggregates := make(map[string]struct {
		TotalRuns  int
		Failures   int
		LastResult TestResult
	})

	for _, res := range allResults {
		key := fmt.Sprintf("%s/%s", res.Package, res.Name) // Unique key for test
		agg := testAggregates[key]
		agg.TotalRuns++
		if res.Status == "FAIL" {
			agg.Failures++
		}
		agg.LastResult = res // Keep track of the last result to get package name
		testAggregates[key] = agg
	}

	var flakyTests []FlakyTest
	for key, agg := range testAggregates {
		if agg.TotalRuns == 0 {
			continue // Should not happen if test is in map
		}
		failureRate := float64(agg.Failures) / float64(agg.TotalRuns)

		if failureRate > threshold && agg.Failures > 0 { // Must have at least one failure
			// Extract package and name from the key or last result
			parts := strings.SplitN(key, "/", 2)
			pkg := ""
			name := key
			if len(parts) == 2 {
				pkg = parts[0]
				name = parts[1]
			} else if agg.LastResult.Package != "" {
				pkg = agg.LastResult.Package
				name = agg.LastResult.Name
			}

			flakyTests = append(flakyTests, FlakyTest{
				Name:           name,
				Package:        pkg,
				FlakinessScore: failureRate,
				TotalRuns:      agg.TotalRuns,
				Failures:       agg.Failures,
			})
		}
	}

	// Sort flaky tests by flakiness score in descending order
	sort.Slice(flakyTests, func(i, j int) bool {
		return flakyTests[i].FlakinessScore > flakyTests[j].FlakinessScore
	})

	return flakyTests
}

// analyzeCmd represents the analyze command
var analyzeCmd = &cobra.Command{
	Use:   "analyze [file...]",
	Short: "Analyze Go test output files for flakiness",
	Long: `Analyzes one or more Go test output files (e.g., from 'go test -v')
to identify potentially flaky tests based on their historical failure rates.

Example:
  go test -v ./... > test_output.log
  flaky-test-tracker analyze test_output.log

Or pipe directly:
  go test -v ./... | flaky-test-tracker analyze
`,
	Args: cobra.ArbitraryArgs, // Allows 0 or more arguments (files)
	Run: func(cmd *cobra.Command, args []string) {
		var readers []io.Reader

		if len(args) == 0 {
			// No files provided, read from stdin
			readers = append(readers, os.Stdin)
			fmt.Println("Reading from stdin. Press Ctrl+D (Unix) or Ctrl+Z then Enter (Windows) to finish input.")
		} else {
			// Read from specified files
			for _, filePath := range args {
				file, err := os.Open(filePath)
				if err != nil {
					log.Fatalf("Error opening file %s: %v", filePath, err)
				}
				defer file.Close()
				readers = append(readers, file)
			}
		}

		var allTestResults []TestResult
		for _, reader := range readers {
			results, err := parseGoTestOutput(reader)
			if err != nil {
				log.Fatalf("Error parsing test output: %v", err)
			}
			allTestResults = append(allTestResults, results...)
		}

		if len(allTestResults) == 0 {
			fmt.Println("No test results found to analyze.")
			return
		}

		// Placeholder for a configurable threshold
		flakinessThreshold := 0.1 // 10% failure rate

		flakyTests := calculateFlakiness(allTestResults, flakinessThreshold)

		fmt.Println("\n--- Flaky Test Report ---")
		if len(flakyTests) == 0 {
			fmt.Printf("No tests identified as flaky (threshold: %.0f%% failure rate).\n", flakinessThreshold*100)
		} else {
			fmt.Printf("Identified %d potentially flaky tests (threshold: %.0f%% failure rate):\n", len(flakyTests), flakinessThreshold*100)
			fmt.Printf("%-50s %-20s %-15s %-10s %-10s\n", "TEST NAME", "PACKAGE", "FLAKINESS SCORE", "FAILURES", "TOTAL RUNS")
			fmt.Println(strings.Repeat("-", 105))
			for _, ft := range flakyTests {
				fmt.Printf("%-50s %-20s %-15.2f%% %-10d %-10d\n",
					ft.Name, ft.Package, ft.FlakinessScore*100, ft.Failures, ft.TotalRuns)
			}
		}
	},
}

// rerunCmd represents the rerun command
var rerunCmd = &cobra.Command{
	Use:   "rerun <test-name> [flags]",
	Short: "Rerun a specific Go test multiple times to confirm flakiness",
	Long: `This command simulates rerunning a specific Go test a given number of times.
In a real implementation, this would execute 'go test -run <test-name>'
and capture its output for analysis.

Example:
  flaky-test-tracker rerun TestMyFlakyFunction -n 100 -p github.com/my/repo/pkg
`,
	Args: cobra.ExactArgs(1), // Requires exactly one argument: the test name
	Run: func(cmd *cobra.Command, args []string) {
		testName := args[0]
		numRuns, _ := cmd.Flags().GetInt("num-runs")
		packageName, _ := cmd.Flags().GetString("package")

		fmt.Printf("Simulating rerunning test '%s' from package '%s' %d times...\n", testName, packageName, numRuns)
		fmt.Println("This is a placeholder. In a full implementation, this would:")
		fmt.Println("1. Build your Go project.")
		fmt.Println("2. Execute 'go test -run ^" + testName + "$ " + packageName + "' repeatedly.")
		fmt.Println("3. Capture and analyze the output of each run.")
		fmt.Println("4. Report on the consistency of the test's outcome over these runs.")
		fmt.Println("\nFor example, you could use 'os/exec' to run 'go test':")
		fmt.Println("  cmd := exec.Command(\"go\", \"test\", \"-v\", \"-run\", \"^\"+testName+\"$\", packageName)")
		fmt.Println("  output, err := cmd.CombinedOutput()")
		fmt.Println("  // Process output and errors")
		fmt.Println("\nConsider using a container (e.g., Docker) for isolated reruns for better reliability.")
	},
}

func init() {
	// Add flags to the rerun command
	rerunCmd.Flags().IntP("num-runs", "n", 10, "Number of times to rerun the test")
	rerunCmd.Flags().StringP("package", "p", "./...", "Go package containing the test (e.g., github.com/user/repo/pkg or ./...)")

	// Add commands to the root command
	rootCmd.AddCommand(analyzeCmd)
	rootCmd.AddCommand(rerunCmd)
}

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "flaky-test-tracker",
	Short: "A tool to track and monitor flaky Go tests",
	Long: `flaky-test-tracker is a CLI tool designed to help identify and manage
flaky tests in your Go projects. It analyzes test output to detect
unstable tests and provides utilities for further investigation.`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func main() {
	Execute()
}
