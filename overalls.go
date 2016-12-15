package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	helpString = `
usage: overalls -project=[path] -covermode[mode] OPTIONS

overalls recursively traverses your projects directory structure
running 'go test -covermode=count -coverprofile=profile.coverprofile'
in each directory with go test files, concatenates them into one
coverprofile in your root directory named 'overalls.coverprofile'

OPTIONS
  -project
	Your project path relative to the '$GOPATH/src' directory
	example: -project=github.com/bluesuncorp/overalls

  -covermode
    Mode to run when testing files.
    default:count

OPTIONAL

  -ignore
    A comma separated list of directory names to ignore, relative to project path.
    example: -ignore=[.git,.hiddentdir...]
    default: '.git,vendor'

  -debug
    A flag indicating whether to print debug messages.
    example: -debug
    default:false

  -concurrency
    Limit the number of packages being processed at one time.
    The minimum value must be 2 or more when set.
    example: -concurrency=5
    default: unlimited
`
)

const (
	defaultIgnores = ".git,vendor"
	outFilename    = "overalls.coverprofile"
	pkgFilename    = "profile.coverprofile"
)

var (
	modeRegex       = regexp.MustCompile("mode: [a-z]+\n")
	gopath          = filepath.Clean(os.Getenv("GOPATH"))
	srcPath         = gopath + "/src/"
	projectPath     string
	ignoreFlag      string
	projectFlag     string
	coverFlag       string
	helpFlag        bool
	debugFlag       bool
	concurrencyFlag int
	isLimited       bool
	emptyStruct     struct{}
	ignores         = map[string]struct{}{}
)

func help() {
	fmt.Printf(helpString)
}

func init() {
	flag.StringVar(&projectFlag, "project", "", "-project [path]: relative to the '$GOPATH/src' directory")
	flag.StringVar(&coverFlag, "covermode", "count", "Mode to run when testing files")
	flag.StringVar(&ignoreFlag, "ignore", defaultIgnores, "-ignore [dir1,dir2...]: comma separated list of directory names to ignore")
	flag.IntVar(&concurrencyFlag, "concurrency", -1, "-concurrency [int]: number of packages to process concurrently, The minimum value must be 2 or more when set.")
	flag.BoolVar(&debugFlag, "debug", false, "-debug [true|false]")
	flag.BoolVar(&helpFlag, "help", false, "-help")
}

func parseFlags() {
	flag.Parse()

	if helpFlag {
		help()
		os.Exit(0)
	}

	if debugFlag {
		fmt.Println("GOPATH:", gopath)
	}

	if len(gopath) == 0 || gopath == "." {
		fmt.Printf("\n**invalid GOPATH '%s'\n", gopath)
		os.Exit(1)
	}

	fmt.Println("|", projectFlag)
	projectFlag = filepath.Clean(projectFlag)

	if debugFlag {
		fmt.Println("Project Path:", projectFlag)
	}

	if len(projectFlag) == 0 || projectFlag == "." {
		fmt.Printf("\n**invalid project path '%s'\n", projectFlag)
		help()
		os.Exit(1)
	}

	switch coverFlag {
	case "set", "count", "atomic":
	default:
		fmt.Printf("\n**invalid covermode '%s'\n", coverFlag)
		os.Exit(1)
	}

	arr := strings.Split(ignoreFlag, ",")
	for _, v := range arr {
		ignores[v] = emptyStruct
	}

	isLimited = concurrencyFlag != -1

	if isLimited && concurrencyFlag < 2 {
		fmt.Printf("\n**invalid concurrency value '%d', value must be at least 2\n", concurrencyFlag)
		os.Exit(1)
	}
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags)
	runMain(logger)
}

func runMain(logger *log.Logger) {
	parseFlags()

	var err error
	var wd string

	projectPath = srcPath + projectFlag + "/"

	if err = os.Chdir(projectPath); err != nil {
		logger.Printf("\n**invalid project path '%s'\n%s\n", projectFlag, err)
		help()
		os.Exit(1)
	}

	if debugFlag {
		wd, err = os.Getwd()
		if err != nil {
			fmt.Println(err)
		}

		logger.Println("Working DIR:", wd)
	}

	testFiles(logger)
}

func scanOutput(r io.ReadCloser, fn func(...interface{})) {
	defer r.Close()
	bs := bufio.NewScanner(r)
	for bs.Scan() {
		fn(bs.Text())
	}
	if err := bs.Err(); err != nil {
		fn(fmt.Sprintf("Scan error: %v", err.Error()))
	}
}

func processDIR(logger *log.Logger, wg *sync.WaitGroup, fullPath, relPath string, out chan<- []byte, semaphore chan struct{}) {
	defer wg.Done()

	// 1 for "test", 4 for coermode, coverprofile, outputdir, relpath
	args := make([]string, 1, 1+len(flag.Args())+4)
	args[0] = "test"
	args = append(args, flag.Args()...)
	args = append(args, "-covermode="+coverFlag, "-coverprofile="+pkgFilename, "-outputdir="+fullPath+"/", relPath)
	fmt.Printf("Test args: %+v\n", args)

	cmd := exec.Command("go", args...)
	if debugFlag {
		logger.Println("Processing:", strings.Join(cmd.Args, " "))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logger.Fatal("Unable to get process stdout")
	}
	go scanOutput(stdout, logger.Print)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		logger.Fatal("Unable to get process stderr")
	}
	go scanOutput(stderr, logger.Print)

	if err := cmd.Run(); err != nil {
		logger.Fatal("ERROR:", err)
	}

	b, err := ioutil.ReadFile(relPath + "/profile.coverprofile")
	if err != nil {
		logger.Fatal("ERROR:", err)
	}

	out <- b

	if isLimited {
		<-semaphore
	}
}

func testFiles(logger *log.Logger) {

	var semaphore chan struct{}

	if isLimited {
		semaphore = make(chan struct{}, concurrencyFlag)
	}

	out := make(chan []byte)
	wg := &sync.WaitGroup{}

	walker := func(path string, info os.FileInfo, err error) error {

		if !info.IsDir() {
			return nil
		}

		rel := strings.Replace(path, projectPath, "", 1)

		if _, ignore := ignores[rel]; ignore {
			return filepath.SkipDir
		}

		rel = "./" + rel

		if files, err := filepath.Glob(rel + "/*_test.go"); len(files) == 0 || err != nil {

			if err != nil {
				logger.Fatal("Error checking for test files")
			}

			if debugFlag {
				logger.Println("No Go Test files in DIR:", rel, "skipping")
			}

			return nil
		}

		wg.Add(1)

		if isLimited {
			semaphore <- struct{}{}
		}

		go processDIR(logger, wg, path, rel, out, semaphore)

		return nil
	}

	if err := filepath.Walk(projectPath, walker); err != nil {
		logger.Fatalf("\n**could not walk project path '%s'\n%s\n", projectPath, err)
	}

	go func() {
		wg.Wait()
		close(out)

		if isLimited {
			close(semaphore)
		}
	}()

	buff := bytes.NewBufferString("")

	for cover := range out {
		buff.Write(cover)
	}

	final := buff.String()
	final = modeRegex.ReplaceAllString(final, "")
	final = "mode: " + coverFlag + "\n" + final

	if err := ioutil.WriteFile(outFilename, []byte(final), 0644); err != nil {
		logger.Fatal("ERROR Writing \""+outFilename+"\"", err)
	}
}
