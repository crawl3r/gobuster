package libgobuster

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// SetupFunc is the "setup" function prototype for implementations
type SetupFunc func(*Gobuster) error

// ProcessFunc is the "process" function prototype for implementations
type ProcessFunc func(*Gobuster, string) ([]Result, error)

// ResultToStringFunc is the "to string" function prototype for implementations
type ResultToStringFunc func(*Gobuster, *Result) (*string, error)

// Gobuster is the main object when creating a new run
type Gobuster struct {
	Opts             *Options
	context          context.Context
	requestsExpected int
	requestsIssued   int
	mu               *sync.RWMutex
	plugin           GobusterPlugin
	resultChan       chan Result
	errorChan        chan error
	LogInfo          *log.Logger
	LogError         *log.Logger
}

// NewGobuster returns a new Gobuster object
func NewGobuster(c context.Context, opts *Options, plugin GobusterPlugin) (*Gobuster, error) {
	var g Gobuster
	g.Opts = opts
	g.plugin = plugin
	g.mu = new(sync.RWMutex)
	g.context = c
	g.resultChan = make(chan Result)
	g.errorChan = make(chan error)
	g.LogInfo = log.New(os.Stdout, "", log.LstdFlags)
	g.LogError = log.New(os.Stderr, "[ERROR] ", log.LstdFlags)

	return &g, nil
}

// Results returns a channel of Results
func (g *Gobuster) Results() <-chan Result {
	return g.resultChan
}

// Errors returns a channel of errors
func (g *Gobuster) Errors() <-chan error {
	return g.errorChan
}

func (g *Gobuster) incrementRequests() {
	g.mu.Lock()
	g.requestsIssued++
	g.mu.Unlock()
}

// PrintProgress outputs the current wordlist progress to stderr
func (g *Gobuster) PrintProgress() {
	if !g.Opts.Quiet && !g.Opts.NoProgress {
		g.mu.RLock()
		if g.Opts.Wordlist == "-" {
			fmt.Fprintf(os.Stderr, "\rProgress: %d", g.requestsIssued)
			// only print status if we already read in the wordlist
		} else if g.requestsExpected > 0 {
			fmt.Fprintf(os.Stderr, "\rProgress: %d / %d (%3.2f%%)", g.requestsIssued, g.requestsExpected, float32(g.requestsIssued)*100.0/float32(g.requestsExpected))
		}
		g.mu.RUnlock()
	}
}

// ClearProgress removes the last status line from stderr
func (g *Gobuster) ClearProgress() {
	fmt.Fprint(os.Stderr, resetTerminal())
}

func (g *Gobuster) worker(wordChan <-chan string, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-g.context.Done():
			return
		case word, ok := <-wordChan:
			// worker finished
			if !ok {
				return
			}
			g.incrementRequests()

			wordCleaned := strings.TrimSpace(word)
			// Skip "comment" (starts with #), as well as empty lines
			if strings.HasPrefix(wordCleaned, "#") || len(wordCleaned) == 0 {
				break
			}

			// Mode-specific processing
			res, err := g.plugin.Run(wordCleaned)
			if err != nil {
				// do not exit and continue
				g.errorChan <- err
				continue
			} else {
				for _, r := range res {
					g.resultChan <- r
				}
			}

			select {
			case <-g.context.Done():
			case <-time.After(g.Opts.Delay):
			}
		}
	}
}

// getWordlist() converted to return multiple scanners instead of one. This allows a directory of wordlists to be loaded \o/
func (g *Gobuster) getWordlist() (*[]bufio.Scanner, error) {
	if g.Opts.Wordlist == "-" {
		// Read directly from stdin
		// return bufio.NewScanner(os.Stdin)
		scanner := bufio.NewScanner(os.Stdin)
		scannerarray := []bufio.Scanner{}
		scannerarray = append(scannerarray, *scanner)
		return &scannerarray, nil
	}

	// check if wordlist is a directory or a file
	fi, err := os.Stat(g.Opts.Wordlist)
	if err != nil {
		return nil, fmt.Errorf("Failed to stat wordlist: %v", err)
	}

	mode := fi.Mode()

	// is file
	if mode.IsRegular() {
		// Pull content from the wordlist
		wordlist, err := os.Open(g.Opts.Wordlist)
		if err != nil {
			return nil, fmt.Errorf("failed to open wordlist: %v", err)
		}

		lines, err := lineCounter(wordlist)
		if err != nil {
			return nil, fmt.Errorf("failed to get number of lines: %v", err)
		}

		g.requestsExpected = lines
		g.requestsIssued = 0

		// rewind wordlist
		_, err = wordlist.Seek(0, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to rewind wordlist: %v", err)
		}
		scanner := bufio.NewScanner(os.Stdin)
		scannerarray := []bufio.Scanner{}
		scannerarray = append(scannerarray, *scanner)
		return &scannerarray, nil
	}

	// if we didn't return out the above block, we must be looking at a directory

	// get all files in directory, just assume they are all wordlists (user's responsibility) -> doesn't do sub-dirs at this time, change to walkpath?
	files, err := ioutil.ReadDir(g.Opts.Wordlist)
	if err != nil {
		return nil, fmt.Errorf("failed to read wordlist directory: %v", err)
	}

	// start building our array of scanners
	scannerarray := []bufio.Scanner{}
	lines := 0

	// pretty happy with how this turned out, seems to work fine - didn't realise it would be a thing!
	for _, f := range files {
		// Pull content from the wordlist
		filewords, err := os.Open(g.Opts.Wordlist + "/" + f.Name())
		if err != nil {
			return nil, fmt.Errorf("failed to open wordlist: %v", err)
		}

		// get current file line count
		templines, err := lineCounter(filewords)
		if err != nil {
			return nil, fmt.Errorf("failed to get number of lines: %v", err)
		}

		// add to total line count
		lines += templines

		// rewind current wordlist
		_, err = filewords.Seek(0, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to rewind wordlist: %v", err)
		}

		filescanner := bufio.NewScanner(filewords)
		scannerarray = append(scannerarray, *filescanner)
	}

	g.requestsExpected = lines
	g.requestsIssued = 0

	return &scannerarray, nil
}

// Start the busting of the website with the given
// set of settings from the command line.
func (g *Gobuster) Start() error {
	defer close(g.resultChan)
	defer close(g.errorChan)

	if err := g.plugin.PreRun(); err != nil {
		return err
	}

	var workerGroup sync.WaitGroup
	workerGroup.Add(g.Opts.Threads)

	wordChan := make(chan string, g.Opts.Threads)

	// Create goroutines for each of the number of threads
	// specified.
	for i := 0; i < g.Opts.Threads; i++ {
		go g.worker(wordChan, &workerGroup)
	}

	// This now return's multiple wordlists, but will only be 1 scanner if 1 wordlist -> multiple if wordlist directory chosen on CLI
	scanners, err := g.getWordlist()
	if err != nil {
		return err
	}

Scan:
	// Is this derpy?! In theory it will work and complete 1 wordlist at a time in the order of the files?
	// Update: doesn't seem as derpy as I thought. Seems to work fine during my test runs. Someone else confirm.
	// Memory might be mental if like 10 MASSIVE lists are loaded though - but users be users.
	for _, s := range *scanners {
		for s.Scan() {
			select {
			case <-g.context.Done():
				break Scan
			case wordChan <- s.Text():
			}
		}
	}
	close(wordChan)
	workerGroup.Wait()
	return nil
}

// GetConfigString returns the current config as a printable string
func (g *Gobuster) GetConfigString() (string, error) {
	return g.plugin.GetConfigString()
}
