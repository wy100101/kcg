package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"

	log "github.com/sirupsen/logrus"

	"github.com/Masterminds/sprig/v3"
	"github.com/alecthomas/kingpin"
	"gopkg.in/yaml.v2"
)

var (
	configFile = kingpin.Flag("file.config", "kcg configuration file").Short('f').ExistingFile()
	debug      = kingpin.Flag("loglevel.debug", "Debug").Short('d').Bool()
)

type config struct {
	BaseDir           string                            `yaml:"base_dir"`
	LeftDelim         string                            `yaml:"left_delim"`
	RightDelim        string                            `yaml:"right_delim"`
	Clusters          []cluster                         `yaml:"clusters"`
	MultiStageEnabled bool                              `yaml:"multi_stage_enabled"`
	DefaultStage      string                            `yaml:"default_stage,omitempty"`
	SourceBases       map[string]map[string]string      `yaml:"source_bases,omitempty"`
	ValuesBases       map[string]map[string]interface{} `yaml:"values_bases,omitempty"`
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

type dependsOn struct {
	Name string `yaml:"name"`
}
type sourceRef struct {
	Kind string `yaml:"kind"`
	Name string `yaml:"name"`
}

type kustomizationSpec struct {
	Interval  string      `yaml:"interval"`
	Force     bool        `yaml:"force"`
	Prune     bool        `yaml:"prune"`
	Path      string      `yaml:"path"`
	SourceRef sourceRef   `yaml:"sourceRef"`
	DependsOn []dependsOn `yaml:"dependsOn,omitempty"`
}

type kustomizationMetadata struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// kustomization source for flux2 source controller
type kustomizationSource struct {
	APIVersion string                `yaml:"apiVersion"`
	Kind       string                `yaml:"kind"`
	Metadata   kustomizationMetadata `yaml:"metadata"`
	Spec       kustomizationSpec     `yaml:"spec"`
}

// / kustomization config for kustomize
type kustomizationConfig struct {
	APIVersion        string            `yaml:"apiVersion"`
	Kind              string            `yaml:"kind"`
	CommonAnnotations map[string]string `yaml:"commonAnnotations"`
	Bases             []string          `yaml:"bases,omitempty"`
	Resources         []string          `yaml:"resources,omitempty"`
}

type cluster struct {
	Platform          string                 `yaml:"platform"`
	Region            string                 `yaml:"region"`
	Env               string                 `yaml:"env"`
	Cluster           string                 `yaml:"cluster"`
	Sources           map[string]string      `yaml:"sources"`
	StaticValues      map[string]interface{} `yaml:"static_values,omitempty"`
	DynamicValues     map[string]string      `yaml:"dynamic_values,omitempty"`
	MultiStageEnabled *bool                  `yaml:"multi_stage_enabled,omitempty"` // use a pointer to allow testing if unset
}

func (c *cluster) Values() map[string]interface{} {
	vs := make(map[string]interface{})
	vs["platform"] = c.Platform
	vs["region"] = c.Region
	vs["env"] = c.Env
	vs["cluster"] = c.Cluster
	vs["sources"] = c.Sources
	for k, v := range c.StaticValues {
		vs[k] = v
	}

	for k, v := range c.DynamicValues {
		t := template.New(fmt.Sprintf("dynamic_values_%s", k))
		sb := strings.Builder{}
		t, err := t.Funcs(sprig.TxtFuncMap()).Parse(v)
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
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		return false, nil
	}
	return i.Mode().IsRegular(), nil
}

func isDir(path string) (bool, error) {
	i, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return i.IsDir(), nil
}

func pruneEmptyDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	entries, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return err
	}
	log.Debug("(pruneEmptyDir) ", dir, " entry count: ", len(entries))
	if len(entries) == 0 {
		return os.RemoveAll(dir)
	}
	return nil
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
		log.Debug("(cleanDir) removing: ", filepath.Join(dir, entry))
		err = os.RemoveAll(filepath.Join(dir, entry))
		if err != nil {
			return err
		}
	}
	return nil
}

func loadConfig(cfp string) (*config, error) {
	c := config{}
	cfb, err := os.ReadFile(cfp)
	if err != nil {
		return &c, err
	}
	err = yaml.Unmarshal(cfb, &c)
	return &c, err
}

// expandTemplate to a buffer and write to file if not empty
func expandTemplate(templateFilePath, outputFilePath, leftDelim, rightDelim string, values map[string]interface{}) error {
	var buf bytes.Buffer
	tfd, err := os.ReadFile(templateFilePath)
	if err != nil {
		return err
	}

	t, err := template.New(templateFilePath).Delims(leftDelim, rightDelim).Funcs(sprig.TxtFuncMap()).Parse(string(tfd))
	if err != nil {
		return err
	}
	err = t.Execute(&buf, values)
	if err != nil {
		return err
	}
	if len(buf.String()) > 0 {
		of, err := os.OpenFile(outputFilePath, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			return err
		}
		defer of.Close()
		log.Debug("(expandTemplate) in: ", templateFilePath, " out: ", outputFilePath, " Size: ", len(buf.String()))
		_, err = of.Write(buf.Bytes())
		if err != nil {
			return err
		}
	}
	return nil
}

func generateKustomizeBases(dir string) ([]string, error) {
	bases := []string{}
	d, err := os.Open(dir)
	if err != nil {
		return []string{}, err
	}
	defer d.Close()
	entries, err := d.Readdirnames(-1)
	if err != nil {
		return []string{}, err
	}
	for _, entry := range entries {
		dt, err := isDir(filepath.Join(dir, entry))
		if err != nil {
			return []string{}, err
		}
		if dt {
			ft, err := isFile(filepath.Join(dir, entry, "kustomization.yaml"))
			if err != nil {
				return []string{}, err
			}
			if ft {
				bases = append(bases, entry)
			}
		}
	}
	sort.Strings(bases)

	return bases, nil
}

func createKustomizationSourceFile(kustomizeDir, destDir string, dependencies []dependsOn) error {
	stage := filepath.Base(kustomizeDir)
	ks := kustomizationSource{
		Kind:       "Kustomization",
		APIVersion: "kustomize.toolkit.fluxcd.io/v1",
		Metadata: kustomizationMetadata{
			Name:      stage,
			Namespace: "flux-system",
		},
		Spec: kustomizationSpec{
			Interval: "1m",
			Path:     fmt.Sprintf("./%s", kustomizeDir),
			Prune:    true,
			Force:    false,
			SourceRef: sourceRef{
				Kind: "GitRepository",
				Name: "flux-system",
			},
		},
	}
	if len(dependencies) > 0 {
		ks.Spec.DependsOn = dependencies
	}
	ksy, err := yaml.Marshal(&ks)
	if err != nil {
		return err
	}
	ksy = []byte(fmt.Sprintf("---\n%s", ksy))
	ksfp := filepath.Join(destDir, fmt.Sprintf("%s.ks.yaml", stage))
	err = os.WriteFile(ksfp, ksy, 0666)
	if err != nil {
		return err
	}
	return nil
}

func createkustomizationConfigFile(kustomizeBaseDir string, cluster cluster, generateBases bool, resources []string) error {
	log.Debug("(createkustomizationConfigFile) for dir: ", kustomizeBaseDir)
	kf := kustomizationConfig{
		Kind:       "Kustomization",
		APIVersion: "kustomize.config.k8s.io/v1beta1",
		CommonAnnotations: map[string]string{
			"platform":    cluster.Platform,
			"region":      cluster.Region,
			"environment": cluster.Env,
			"cluster":     cluster.Cluster,
		},
		Bases:     nil,
		Resources: nil,
	}
	if generateBases {
		bases, err := generateKustomizeBases(kustomizeBaseDir)
		if err != nil {
			return err
		}
		if len(bases) > 0 {
			kf.Bases = bases
		}
	}
	if len(resources) > 0 {
		kf.Resources = resources
	}
	// only create kustomization.yaml if there are resources or bases
	if kf.Resources != nil || kf.Bases != nil {
		log.Debug("(createkustomizationConfigFile) ", kustomizeBaseDir, "kustomization.yaml... writing!")
		kfy, err := yaml.Marshal(&kf)
		if err != nil {
			return err
		}
		kfy = []byte(fmt.Sprintf("---\n%s", kfy))
		kfp := filepath.Join(kustomizeBaseDir, "kustomization.yaml")
		err = os.WriteFile(kfp, kfy, 0666)
		if err != nil {
			return err
		}
	} else {
		log.Debug("(createkustomizationConfigFile) no resources or bases found.")
		log.Debug("(createkustomizationConfigFile) ", kustomizeBaseDir, "kustomization.yaml... skipping!")
	}
	return nil
}

func processSource(sourceDir, destDir, leftDelim, rightDelim string, values map[string]interface{}, multiStageEnabled, topLevel bool) error {
	log.Debug("(ProcessSource)", " sourceDir: ", sourceDir, " destDir: ", destDir, " multiStageEnabled ", multiStageEnabled, " topLevel ", topLevel)
	// the top level destination directory won't be correct for multistage at the top level skip create
	if !(topLevel && multiStageEnabled) {
		skfp := filepath.Join(sourceDir, "kustomization.yaml")
		skfe, err := isFile(skfp)
		if err != nil {
			return err
		}
		if !skfe {
			return fmt.Errorf("%s exists but is not a regular file", skfp)
		}
		err = os.MkdirAll(destDir, 0777)
		defer pruneEmptyDir(destDir)
		if err != nil {
			return fmt.Errorf("Failed to create directory %s (%s)", destDir, err)
		}
		err = cleanDir(destDir)
		if err != nil {
			panic(err)
		}
	}
	ses, err := os.ReadDir(sourceDir)
	if err != nil {
		return err
	}
	mdd := destDir // default modified destination directory is destDir
	ddb := filepath.Base(destDir)
	ddd := filepath.Dir(destDir)
	sd := "10" // default stage for top level resources
	if topLevel && multiStageEnabled {
		// update destination dir to be /cluster-dir/01/source if top level
		mdd = filepath.Join(ddd, sd, ddb)
	}
	for _, se := range ses {
		if se.IsDir() {
			if topLevel && multiStageEnabled {
				// if top level and the directory is a number update the destination for multistage
				matched, err := regexp.MatchString(`^\d\d$`, se.Name())
				if err != nil {
					return err
				}
				// if the source entry is a number, then use the
				if matched {
					sd = se.Name()
				}
				mdd = filepath.Join(ddd, sd, ddb)
			}
			// if source entry is a directory call processSource on it
			err = processSource(filepath.Join(sourceDir, se.Name()), mdd, leftDelim, rightDelim, values, multiStageEnabled, false)
			if err != nil {
				return err
			}
		} else {
			// process the file like a template
			err = os.MkdirAll(mdd, 0777)
			if err != nil {
				return err
			}
			err = expandTemplate(filepath.Join(sourceDir, se.Name()), filepath.Join(mdd, se.Name()), leftDelim, rightDelim, values)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func processCluster(c cluster, cfg *config, wg *sync.WaitGroup) {
	log.Debug("(processingCluster):", c.Cluster)
	defer wg.Done()
	mse := cfg.MultiStageEnabled
	if c.MultiStageEnabled != nil {
		mse = *c.MultiStageEnabled
	}

	v := c.Values()
	v["clusters"] = cfg.Clusters
	cd := filepath.Join(cfg.BaseDir, c.Platform, c.Region, c.Env, c.Cluster)
	err := os.MkdirAll(cd, 0777)
	if err != nil {
		panic(err)
	}
	cleanDir(cd)

	for d, s := range c.Sources {
		v["source_key"] = d
		v["source_path"] = s

		err = processSource(filepath.Join(cfg.BaseDir, s), filepath.Join(cd, d), cfg.LeftDelim, cfg.RightDelim, v, mse, true)
		if err != nil {
			panic(err)
		}
	}
	// clean up empty directories
	cdes, err := os.ReadDir(cd)
	if err != nil {
		panic(err)
	}
	for _, cde := range cdes {
		if cde.IsDir() {
			err = pruneEmptyDir(filepath.Join(cd, cde.Name()))
			if err != nil {
				panic(err)
			}
		}
	}

	if mse {
		cdes, err := os.ReadDir(cd)
		if err != nil {
			panic(err)
		}
		// create kustomization.yaml for each directory and corresponding ks
		stages := []string{}
		for _, cde := range cdes {
			if cde.IsDir() {
				matched, err := regexp.MatchString(`^\d\d$`, cde.Name())
				if err != nil {
					panic(err)
				}
				if matched {
					stages = append(stages, cde.Name())
					err = createkustomizationConfigFile(filepath.Join(cd, cde.Name()), c, true, nil)
					if err != nil {
						panic(err)
					}
				}
			}
		}
		sort.Strings(stages)
		ds := ""
		sksfs := []string{}
		for _, s := range stages {
			err = createkustomizationConfigFile(filepath.Join(cd, s), c, true, nil)
			if err != nil {
				panic(err)
			}
			if len(ds) > 0 {
				err = createKustomizationSourceFile(filepath.Join(cd, s), cd, []dependsOn{{Name: ds}})
			} else {
				err = createKustomizationSourceFile(filepath.Join(cd, s), cd, nil)
			}
			if err != nil {
				panic(err)
			}
			sksfs = append(sksfs, fmt.Sprintf("%s.ks.yaml", s))
			ds = filepath.Join(cd, s)
			ds = s // next ks should depend on this one
		}
		err = createkustomizationConfigFile(cd, c, false, sksfs)
		if err != nil {
			panic(err)
		}
	} else {
		err = createkustomizationConfigFile(cd, c, true, nil)
		if err != nil {
			panic(err)
		}
	}
}

func cloneGitSource(src_url string) (git_clone string, err error) {
	return src_url, nil
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
