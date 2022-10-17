package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"text/template"

	yaml "gopkg.in/yaml.v2"
)

type Config struct {
	Host     string
	User     string
	UserHost string
	Values   AnyConfig
}

type AnyConfig map[string]interface{}

const defaultConfig = `HiDpi: true
InDocker: false
`

const extension = "tmpl"

var flags *Flags
var t *template.Template = template.New("templater")
var templRegEx *regexp.Regexp = regexp.MustCompile("^.*(\\." + extension + ")(\\.|$)")

func nicePath(path string) string {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get working dir: %s", err)
	}

	out, err := filepath.Rel(cwd, path)
	if err != nil {
		panic(err.Error())
	}

	return out
}

func scan() {

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get working dir: %s", err)
	}

	e := filepath.Walk(cwd, func(p string, info os.FileInfo, err error) error {
		// TODO: make this configurable
		if info.IsDir() && info.Name() == ".templater" {
			return filepath.SkipDir
		}
		if err == nil && templRegEx.MatchString(info.Name()) {
			scannedPath := p

			if !path.IsAbs(scannedPath) {
				panic(scannedPath + " is not absolute")
			}

			outputPath := convertOutputPath(scannedPath)

			r, err := flags.inputReader(scannedPath)
			if err != nil {
				log.Printf("Failed to open for reading %s: %s ... skipping", nicePath(scannedPath), err.Error())
				return nil
			}

			var mode string
			b := new(bytes.Buffer)
			err = executeTemplate(r, b)
			if err != nil {
				log.Printf("Failed to execute template %s:\n    %s\n", nicePath(scannedPath), err.Error())
				mode = "FAIL"
			} else {
				existing, err := flags.getExistingOutputFileContents(outputPath)
				if err != nil {
					if !os.IsNotExist(err) {
						log.Printf("Unexpected error getting existing output contents: %s", err.Error())
					}
					mode = "CREATE"
				} else {
					if !reflect.DeepEqual(b.Bytes(), existing.Bytes()) {
						mode = "MODIFY"
					} else {
						mode = "KEEP"
					}
				}
			}

			if mode == "MODIFY" || mode == "CREATE" {

				w, err := flags.outputWriter(outputPath)
				if err != nil {
					if err == skipReplace {
						// skip quietly: user just confirmed
						return nil
					}
					log.Printf("Failed to create file %s: %s ... skipping", nicePath(outputPath), err.Error())
					return nil
				}

				_, err = io.Copy(w, b)
				if err != nil {
					log.Printf("Unexpected error copying file: %s", err)
					mode = "FAIL"
				}
				if *flags.readOnly {
					err = markFileReadOnly(outputPath)
					if err != nil {
						log.Printf("Failed to mark output path read only: %s", err.Error())
					}
				}
			}

			if *flags.porcelain {
				fmt.Printf("%s\t%s\t%s\n", mode, nicePath(scannedPath), nicePath(outputPath))
			} else {
				switch mode {
				case "KEEP":
					fmt.Printf("No change made to %s. Skipping.\n", nicePath(outputPath))
				case "MODIFY":
					fmt.Printf("Re-writing %s to %s.\n", nicePath(scannedPath), nicePath(outputPath))
				case "CREATE":
					fmt.Printf("Writing %s to new file %s.\n", nicePath(scannedPath), nicePath(outputPath))
				case "FAIL":
					fmt.Printf("Failed to process %s. Skipping.\n", nicePath(scannedPath))
				}
			}
		}
		return nil
	})
	if e != nil {
		log.Fatal(e)
	}

}

func configFile() string {
	configPath := os.Getenv("TEMPLATER_CONFIG")
	if configPath == "" {
		configPath = os.ExpandEnv("$HOME/.config/templater/config")
	}
	_, err := os.Stat(configPath)

	if os.IsNotExist(err) {
		dir := path.Dir(configPath)
		err := os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			panic(err.Error())
		}
		f, err := os.Create(configPath)
		if err != nil {
			panic(err.Error())
		}
		_, err = f.WriteString(defaultConfig)
		if err != nil {
			panic(err.Error())
		}

	}

	return configPath
}

var cachedConfig *Config = nil

func config() Config {
	if cachedConfig != nil {
		return *cachedConfig
	}

	configFile := configFile()
	bs, err := ioutil.ReadFile(configFile)
	if err != nil {
		panic(err.Error())
	}

	exConfig := make(AnyConfig)
	err = yaml.UnmarshalStrict(bs, &exConfig)
	if err != nil {
		log.Fatal(err.Error())
	}

	config := Config{Values: exConfig}

	hostName, err := os.Hostname()
	if err != nil {
		panic(err.Error())
	}
	user, err := user.Current()
	if err != nil {
		panic(err.Error())
	}

	if *flags.hostOverride != "" {
		config.Host = *flags.hostOverride
	} else {
		config.Host = hostName
	}
	if *flags.userOverride != "" {
		config.User = *flags.userOverride
	} else {
		config.User = user.Username
	}
	config.UserHost = config.User + "@" + config.Host

	cachedConfig = &config
	return config
}

func stripTempl(base string) string {
	return strings.Replace(base, "."+extension, "", 1)
}

// if scanned path assume input path from flags
// return the input param if can't convert
// DOES NOT consult flags.out / up to caller
func convertOutputPath(scannedPath string) string {
	var (
		inputDir   string
		inputBase  string
		outputDir  string
		origParent string
		newParent  string
	)

	if scannedPath == "" {
		// not scanning
		if *flags.in == "" {
			return ""
		} else {
			var err error
			scannedPath, err = filepath.Abs(*flags.in)
			if err != nil {
				log.Fatalf("Error for %s: %s", *flags.in, err)
			}
		}
	}

	origParent = flags.origParentAbs()
	newParent = flags.newParentAbs()
	scannedPath = path.Clean(scannedPath)
	inputDir = path.Dir(scannedPath)
	inputBase = path.Base(scannedPath)

	canConvertDir := false
	if origParent != "" && newParent != "" {
		canConvertDir = true
		if !strings.HasPrefix(inputDir, origParent) {
			log.Fatalf("Fatal: %s is not under %s", inputDir, origParent)
		}
	}

	if canConvertDir {
		if len(origParent) == len(inputDir) {
			outputDir = newParent
		} else {
			outputDir = newParent + inputDir[len(origParent):]
		}
	} else {
		outputDir = inputDir
	}

	return path.Join(outputDir, stripTempl(inputBase))
}

var skipReplace error = errors.New("should skip")

func markFileReadOnly(outputPath string) error {
	return os.Chmod(outputPath, 0444)
}

func createOutputFile(outputPath string) (io.Writer, error) {
	os.Remove(outputPath)
	dir := path.Dir(outputPath)
	err := os.MkdirAll(dir, os.ModePerm)
	if err != nil {
		return nil, err
	}

	return os.Create(outputPath)
}

func promptAndCreateOutputFile(outputPath string) (io.Writer, error) {
	if flags.shouldDryRun() {
		return ioutil.Discard, nil
	}

	if !flags.shouldPromptBeforeWrite() {
		// no interactive, just try to create
		return createOutputFile(outputPath)
	}

	if _, err := os.Stat(outputPath); err != nil {
		// interactive but does not exist
		return createOutputFile(outputPath)

	} else {
		// interactive / exists : prompt first
		reader := bufio.NewReader(os.Stdin)
		fmt.Printf("Replace %s? [y|n] ", outputPath)
		text, _ := reader.ReadString('\n')
		text = strings.TrimSpace(text)
		if strings.EqualFold(text, "y") || strings.EqualFold(text, "yes") {
			return createOutputFile(outputPath)
		} else {
			return nil, skipReplace
		}

	}
}

type Flags struct {
	scan         *bool
	porcelain    *bool
	dryRun       *bool
	interactive  *bool
	readOnly     *bool
	out          *string
	in           *string
	origParent   *string
	newParent    *string
	hostOverride *string
	userOverride *string
}

func (f *Flags) shouldScan() bool {
	return *f.scan && *f.in == "" && *f.out == ""
}

func (f *Flags) shouldDryRun() bool {
	return *f.dryRun
}

func (f *Flags) shouldPromptBeforeWrite() bool {
	return *f.interactive && !f.isStdin()
}

func (f *Flags) isValid() bool {
	return true
}

func (f *Flags) isStdin() bool {
	return *f.in == "" && !*f.scan
}

func (f *Flags) inputReader(scannedPath string) (io.Reader, error) {
	if scannedPath != "" {
		return os.Open(scannedPath)
	}
	if f.isStdin() {
		return os.Stdin, nil
	} else {
		return os.Open(*f.in)
	}
}

func (f *Flags) getOutputPathForNonScan() string {
	if f.shouldScan() {
		panic("Don't call this if we are scanning")
	}

	if *f.out == "" {
		return convertOutputPath("")
	} else {
		return *f.out
	}
}

func (f *Flags) getExistingOutputFileContents(outputPath string) (*bytes.Buffer, error) {
	if outputPath == "" {
		panic("outputPath required")
	}
	file, err := os.Open(outputPath)
	if err != nil {
		return nil, err
	}
	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, file)
	if err != nil {
		return nil, err
	}

	return buf, nil
}

func (f *Flags) outputWriter(outputPath string) (io.Writer, error) {
	if outputPath != "" {
		return promptAndCreateOutputFile(outputPath)
	}

	outputPath = f.getOutputPathForNonScan()
	if outputPath == "" {
		return os.Stdout, nil
	} else {
		return promptAndCreateOutputFile(outputPath)
	}
}

func (f *Flags) newParentAbs() string {
	abs, err := filepath.Abs(*f.newParent)
	if err != nil {
		log.Fatalf("Error for %s: %s", *f.newParent, err)
	}
	return abs
}

func (f *Flags) origParentAbs() string {
	abs, err := filepath.Abs(*f.origParent)
	if err != nil {
		log.Fatalf("Error for %s: %s", *f.origParent, err)
	}
	return abs
}

func executeTemplate(r io.Reader, w io.Writer) error {
	bs, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}

	templ, err := t.Parse(string(bs))
	if err != nil {
		return err
	}

	config := config()

	return templ.Execute(w, config)
}

func main() {
	flags = &Flags{
		flag.Bool("scan", false, "scan directory recursively for template files (ignored if -in or -out are used)"),
		flag.Bool("p", false, "porcelain: output machine readable tab delimited stdout (-scan only)"),
		flag.Bool("n", false, "dry run (-scan only)"),
		flag.Bool("i", false, "interactive mode / prompt before replacing files (ignored if reading from stdin)"),
		flag.Bool("ro", false, "mark output files as read-only (-scan only)"),
		flag.String("out", "", "output to file (write to stdout otherwise)"),
		flag.String("in", "", "input from file (read from stdin otherwise)"),
		flag.String("orig", "", "original path prefix to be replaced with new"),
		flag.String("new", "", "new path prefix"),
		flag.String("override-host", "", "Override the value provided by .Host"),
		flag.String("override-user", "", "Override the value provided by .User"),
	}

	flag.Parse()

	if !flags.isValid() {
		flag.Usage()
		os.Exit(1)
		return
	}

	if flags.shouldScan() {
		scan()
	} else {
		r, err := flags.inputReader("")
		if err != nil {
			log.Fatalf("Failed to open for reading %s: %s", *flags.in, err.Error())
			return
		}

		w, err := flags.outputWriter("")
		if err != nil {
			log.Fatalf("Failed to create file %s: %s", *flags.out, err.Error())
			return
		}

		if flags.shouldDryRun() {
			read := *flags.in
			if flags.isStdin() {
				read = "<stdin>"
			}

			fmt.Printf("Will read from %s and write to %s\n",
				read, nicePath(flags.getOutputPathForNonScan()))

			if flags.isStdin() {
				// it's kind of weird to do a dry run with stdin
				return
			}
		}

		err = executeTemplate(r, w)
		if err != nil {
			log.Fatalf("Failed execute template: \n    %s", err.Error())
			return
		}
	}
}
