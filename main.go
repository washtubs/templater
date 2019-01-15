package main

import (
	"bufio"
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
	"regexp"
	"strings"
	"text/template"

	yaml "gopkg.in/yaml.v2"
)

type Config struct {
	Host     string
	User     string
	UserHost string
	ExplicitConfig
}

type ExplicitConfig struct {
	HiDpi    bool `yaml:"HiDpi"`
	InDocker bool `yaml:"InDocker"`
}

const defaultConfig = `HiDpi: true
InDocker: false
`

const extension = "tmpl"

var flags *Flags
var t *template.Template = template.New("templater")
var templRegEx *regexp.Regexp = regexp.MustCompile("^.+(\\." + extension + ")(\\.|$)")

func scan() {

	e := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err == nil && templRegEx.MatchString(info.Name()) {
			scannedPath := path
			outputPath := stripTempl(scannedPath)
			if flags.shouldDryRun() {
				fmt.Printf("Will re-write %s to %s\n", scannedPath, outputPath)
			} else {
				r, err := flags.inputReader(scannedPath)
				if err != nil {
					log.Printf("Failed to open for reading %s: %s ... skipping", scannedPath, err.Error())
					return nil
				}
				w, err := flags.outputWriter(outputPath)
				if err != nil {
					if err == skipReplace {
						// skip quietly: user just confirmed
						return nil
					}
					log.Printf("Failed to create file %s: %s ... skipping", outputPath, err.Error())
					return nil
				}
				err = executeTemplate(r, w)
				fmt.Printf("Re-writing %s to %s\n", scannedPath, outputPath)
				if err != nil {
					log.Printf("Failed to re-write %s to %s: %s", scannedPath, outputPath, err.Error())
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
	configPath := os.ExpandEnv("$HOME/.config/templater/config")
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
		f.WriteString(defaultConfig)
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

	exConfig := ExplicitConfig{}
	err = yaml.UnmarshalStrict(bs, &exConfig)
	if err != nil {
		log.Fatal(err.Error())
	}

	config := Config{ExplicitConfig: exConfig}

	hostName, err := os.Hostname()
	if err != nil {
		panic(err.Error())
	}
	user, err := user.Current()
	if err != nil {
		panic(err.Error())
	}

	config.Host = hostName
	config.User = user.Username
	config.UserHost = user.Username + "@" + hostName

	cachedConfig = &config
	return config
}

func stripTempl(path string) string {
	return strings.Replace(path, "."+extension, "", 1)
}

var skipReplace error = errors.New("should skip")

func createOutputFile(outputPath string) (io.Writer, error) {
	if !flags.shouldPromptBeforeWrite() {
		// no interactive, just try to create
		return os.Create(outputPath)
	}

	if _, err := os.Stat(outputPath); err != nil {
		// interactive but does not exist
		return os.Create(outputPath)

	} else {
		// interactive / exists : prompt first
		reader := bufio.NewReader(os.Stdin)
		fmt.Printf("Replace %s? [y|n] ", outputPath)
		text, _ := reader.ReadString('\n')
		text = strings.TrimSpace(text)
		if strings.EqualFold(text, "y") || strings.EqualFold(text, "yes") {
			return os.Create(outputPath)
		} else {
			return nil, skipReplace
		}

	}
}

type Flags struct {
	scan        *bool
	dryRun      *bool
	interactive *bool
	out         *string
	stdout      *bool
	in          *string
	stdin       *bool
}

func (f *Flags) shouldScan() bool {
	return *f.scan && *f.in == "" && *f.out == ""
}

func (f *Flags) shouldDryRun() bool {
	return *f.dryRun
}

func (f *Flags) shouldPromptBeforeWrite() bool {
	return *f.interactive && !*f.stdin
}

func (f *Flags) isValid() bool {
	if *f.scan {
		return true
	}
	if (*f.stdin || *f.in != "") && (*f.stdout || *f.out != "") {
		return true
	}
	return false
}

func (f *Flags) inputReader(scannedPath string) (io.Reader, error) {
	if scannedPath != "" {
		return os.Open(scannedPath)
	}
	if *f.in == "" && !*f.stdin {
		flag.Usage()
		log.Fatal("No input file")
		return nil, nil
	}
	if *f.stdin {
		return os.Stdin, nil
	} else {
		return os.Open(*f.in)
	}
}

func (f *Flags) outputWriter(outputPath string) (io.Writer, error) {
	if outputPath != "" {
		return createOutputFile(outputPath)
	}
	if *f.out == "" {
		// not specified: assume stdout
		return os.Stdout, nil
	}
	if *f.stdout {
		return os.Stdout, nil
	} else {
		return createOutputFile(*f.out)
	}
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
		flag.Bool("n", false, "dry run (ignored is -scan is not specified)"),
		flag.Bool("i", false, "interactive mode / prompt before replacing files (ignored if -stdin is used)"),
		flag.String("out", "", "output to file"),
		flag.Bool("stdout", false, "output to stdout (overrides -out)"),
		flag.String("in", "", "input from file"),
		flag.Bool("stdin", false, "input from stdin (overrides -in)"),
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

		err = executeTemplate(r, w)
		if err != nil {
			log.Fatalf("Failed execute template: %s", err.Error())
			return
		}
	}
}
