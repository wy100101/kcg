package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/template"

	log "github.com/sirupsen/logrus"

	"github.com/alecthomas/kingpin"
	"gopkg.in/yaml.v2"
)

var (
	configFile = kingpin.Flag("file.config", "kcg configuration file").Short('f').ExistingFile()
	debug      = kingpin.Flag("loglevel.debug", "Debug").Short('d').Bool()
)

type config struct {
	BaseDir     string                            `yaml:"base_dir"`
	LeftDelim   string                            `yaml:"left_delim"`
	RightDelim  string                            `yaml:"right_delim"`
	Clusters    []cluster                         `yaml:"clusters"`
	SourceBases map[string]map[string]string      `yaml:"source_bases,omitempty"`
	ValuesBases map[string]map[string]interface{} `yaml:"values_bases,omitempty"`
}

func (c *config) Validate() error {
	if len(c.Clusters) == 0 {
		return fmt.Errorf("No clusters defined")
	}
	if len(c.BaseDir) == 0 {
		return fmt.Errorf("No base_dir defined")
	}
	if len(c.LeftDelim) == 0 {
		return fmt.Errorf("No left_delim defined")
	}
	if len(c.RightDelim) == 0 {
		return fmt.Errorf("No right_delim defined")
	}
	if len(c.RightDelim) != len(c.LeftDelim) {
		return fmt.Errorf("right_delim and left_delim must be the same length")
	}
	return nil
}

type kustomizeFile struct {
	APIVersion        string            `yaml:"apiVersion"`
	Kind              string            `yaml:"kind"`
	CommonAnnotations map[string]string `yaml:"commonAnnotations"`
	Bases             []string          `yaml:"bases"`
}

type cluster struct {
	Platform      string                 `yaml:"platform"`
	Region        string                 `yaml:"region"`
	Env           string                 `yaml:"env"`
	Cluster       string                 `yaml:"cluster"`
	Sources       map[string]string      `yaml:"sources"`
	StaticValues  map[string]interface{} `yaml:"static_values,omitempty"`
	DynamicValues map[string]string      `yaml:"dynamic_values,omitempty"`
}

func (c *cluster) Values() map[string]interface{} {
	var sb strings.Builder
	vs := make(map[string]interface{})
	vs["platform"] = c.Platform
	vs["region"] = c.Region
	vs["env"] = c.Env
	vs["cluster"] = c.Cluster
	for k, v := range c.StaticValues {
		vs[k] = v
	}
	t := template.New("dynamic_values")
	for k, v := range c.DynamicValues {
		t, err := t.Parse(v)
		if err != nil {
			log.Debug("(c.Values) error parsing dynamic_values: ", err)
		}
		err = t.Execute(&sb, vs)
		if err != nil {
			log.Debug("(c.Values) error executing dynamic_values: ", err)
		}
		vs[k] = sb.String()
	}
	return vs
}

func (c *cluster) String() string {
	return fmt.Sprintf("%s-%s-%s-%s [values] %s", c.Platform, c.Region, c.Env, c.Cluster, c.Values())
}

func (c *cluster) Validate() error {
	reserved_values := []string{"platform", "region", "env", "cluster", "source_key", "source_path", "clusters"}
	for _, r := range reserved_values {
		// these values will be overwritten by Values()
		if _, ok := c.StaticValues[r]; ok {
			return fmt.Errorf("'%s' is a reserved var and cannot be overridden", r)
		}
		if _, ok := c.DynamicValues[r]; ok {
			return fmt.Errorf("'%s' is a reserved var and cannot be overridden", r)
		}
	}
	return nil
}

func isFile(path string) (bool, error) {
	i, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return i.Mode().IsRegular(), nil
}

func cleanDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	entries, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		err = os.RemoveAll(filepath.Join(dir, entry))
		if err != nil {
			return err
		}
	}
	return nil
}

func loadConfig(cfp string) (*config, error) {
	c := config{}
	cfb, err := ioutil.ReadFile(cfp)
	if err != nil {
		return &c, err
	}
	err = yaml.Unmarshal(cfb, &c)
	return &c, err
}

func expandTemplate(templateFilePath, outputFilePath, leftDelim, rightDelim string, values map[string]interface{}) error {
	tfd, err := ioutil.ReadFile(templateFilePath)
	if err != nil {
		return err
	}
	of, err := os.OpenFile(outputFilePath, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	defer of.Close()

	t := template.New(templateFilePath).Delims(leftDelim, rightDelim)
	t, err = t.Parse(string(tfd))
	if err != nil {
		return err
	}
	return t.Execute(of, values)
}

func processSource(sourceDir, destDir, leftDelim, rightDelim string, values map[string]interface{}) error {
	log.Debug("(ProcessSource)", " sourceDir: ", sourceDir, " destDir: ", destDir)
	skfp := filepath.Join(sourceDir, "kustomization.yaml")
	skfe, err := isFile(skfp)
	if err != nil {
		return err
	}
	if !skfe {
		return fmt.Errorf("%s exists but is not a regular file", skfp)
	}
	err = os.MkdirAll(destDir, 0777)
	if err != nil {
		return fmt.Errorf("Failed to create directory %s (%s)", destDir, err)
	}
	err = cleanDir(destDir)
	if err != nil {
		panic(err)
	}
	ses, err := os.ReadDir(sourceDir)
	if err != nil {
		return err
	}
	for _, se := range ses {
		if se.IsDir() {
			// if source entry is a directory call processSource on it
			err = processSource(filepath.Join(sourceDir, se.Name()), filepath.Join(destDir, se.Name()), leftDelim, rightDelim, values)
			if err != nil {
				return err
			}
		} else {
			// process the file like a template
			err = expandTemplate(filepath.Join(sourceDir, se.Name()), filepath.Join(destDir, se.Name()), leftDelim, rightDelim, values)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func processCluster(c cluster, cfg *config, wg *sync.WaitGroup) {
	log.Debug("Processing cluster:", c.Cluster)
	defer wg.Done()
	v := c.Values()
	v["clusters"] = cfg.Clusters
	kbs := []string{}
	cd := filepath.Join(cfg.BaseDir, c.Platform, c.Region, c.Env, c.Cluster)
	err := os.MkdirAll(cd, 0777)
	if err != nil {
		panic(err)
	}
	cleanDir(cd)

	for d, s := range c.Sources {
		v["source_key"] = d
		v["source_path"] = s
		kbs = append(kbs, d)

		err = processSource(filepath.Join(cfg.BaseDir, s), filepath.Join(cd, d), cfg.LeftDelim, cfg.RightDelim, v)
		if err != nil {
			panic(err)
		}
	}

	sort.Strings(kbs)

	kf := kustomizeFile{
		Kind:       "Kustomization",
		APIVersion: "kustomize.config.k8s.io/v1beta1",
		CommonAnnotations: map[string]string{
			"platform":    c.Platform,
			"region":      c.Region,
			"environment": c.Env,
			"cluster":     c.Cluster,
		},
		Bases: kbs,
	}
	kfy, err := yaml.Marshal(&kf)
	if err != nil {
		panic(err)
	}
	kfy = []byte(fmt.Sprintf("---\n%s", kfy))
	kfp := filepath.Join(cd, "kustomization.yaml")
	err = ioutil.WriteFile(kfp, kfy, 0666)
	if err != nil {
		panic(err)
	}
}

func main() {
	kingpin.Parse()
	cfg, err := loadConfig(*configFile)
	if *debug {
		log.SetLevel(log.DebugLevel)
	}
	log.Debug("Debug logging enabled!")
	if err != nil {
		log.Panic(err)
	}
	err = cfg.Validate()
	if err != nil {
		log.Panic(err)
	}

	var wg sync.WaitGroup

	for _, c := range cfg.Clusters {
		wg.Add(1)
		if *debug {
			log.Debug("Processing cluster: ", c.String())
			log.Debug("Sources:")
			for k, v := range c.Sources {
				log.Debug("* ", k, ": ", v)
			}
			log.Debug("Values:")
			for k, v := range c.Values() {
				log.Debug("* ", k, ": ", v)
			}
		}

		go processCluster(c, cfg, &wg)
	}

	wg.Wait()
}
