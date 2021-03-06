package ant

import (
	"archive/zip"
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar"
	"github.com/gnewton/jargo"

	"github.com/fossas/fossa-cli/builders/maven"
	"github.com/fossas/fossa-cli/exec"
	"github.com/fossas/fossa-cli/files"
	"github.com/fossas/fossa-cli/log"
	"github.com/fossas/fossa-cli/module"
)

// AntBuilder implements build context for SBT builds
type AntBuilder struct {
	AntCmd     string
	AntVersion string

	JavaCmd     string
	JavaVersion string
}

// Initialize collects metadata on Java and SBT binaries
func (builder *AntBuilder) Initialize() error {
	log.Logger.Debugf("Initializing Ant builder...")

	// Set Java context variables
	javaCmd, javaVersion, err := exec.Which("-version", os.Getenv("JAVA_BINARY"), "java")
	if err != nil {
		log.Logger.Warningf("Could not find Java binary (try setting $JAVA_BINARY): %s", err.Error())
	}
	builder.JavaCmd = javaCmd
	builder.JavaVersion = javaVersion

	// Set Ant context variables
	antCmd, antVersionOut, err := exec.Which("-version", os.Getenv("ANT_BINARY"), "ant")
	if err != nil {
		return fmt.Errorf("could not find Ant binary (try setting $ANT_BINARY): %s", err.Error())
	}

	builder.AntCmd = antCmd
	antVersionMatchRe := regexp.MustCompile(`version ([0-9]+\.[0-9]+.\w+)`)
	match := antVersionMatchRe.FindStringSubmatch(antVersionOut)

	if len(match) == 2 {
		builder.AntVersion = match[1]
	}

	log.Logger.Debugf("Initialized Ant builder: %#v", builder)
	return nil
}

// Build is currently not implemented
func (builder *AntBuilder) Build(m module.Module, force bool) error {
	return errors.New("Ant builder is not implemented")
}

// Analyze resolves a lib directory and parses the jars inside
func (builder *AntBuilder) Analyze(m module.Module, allowUnresolved bool) ([]module.Dependency, error) {
	log.Logger.Debugf("Running Ant analysis: %#v %#v in %s", m, allowUnresolved, m.Dir)

	options := m.Context.(map[string]interface{})

	libdir := "lib"
	if options["libdir"] != nil {
		libdir = options["libdir"].(string)
	}

	log.Logger.Debugf("resolving ant libs in: %s", libdir)
	if ok, err := files.ExistsFolder(m.Dir, libdir); !ok || err != nil {
		return nil, errors.New("unable to resolve library directory, try specifying it using the `modules.options.libdir` property in `fossa.yml`")
	}

	jarFilePaths, err := doublestar.Glob(filepath.Join(libdir, "*.jar"))
	if err != nil {
		return nil, err
	}

	log.Logger.Debugf("Running Ant analysis: %#v", jarFilePaths)

	var dependencies []module.Dependency

	// traverse through libdir and and resolve jars
	for _, jarFilePath := range jarFilePaths {
		locator, err := locatorFromJar(jarFilePath)
		if err == nil {
			dependencies = append(dependencies, module.Dependency{
				Locator: locator,
			})
		} else {
			log.Logger.Warningf("unable to resolve Jar: %s", jarFilePath)
		}
	}

	return dependencies, nil
}

func getPOMFromJar(path string) (maven.POMFile, error) {
	var pomFile maven.POMFile

	log.Logger.Debugf(path)
	if path == "" {
		return pomFile, errors.New("invalid POM path specified")
	}

	jarFile, err := os.Open(path)
	if err != nil {
		return pomFile, err
	}

	defer jarFile.Close()

	zfi, err := jarFile.Stat()
	if err != nil {
		return pomFile, err
	}

	zr, err := zip.NewReader(jarFile, zfi.Size())
	if err != nil {
		return pomFile, err
	}

	for _, f := range zr.File {
		// decode a single pom.xml directly from jar
		if f.Name == path {
			rc, err := f.Open()
			if err != nil {
				return pomFile, err
			}
			defer rc.Close()

			reader := bufio.NewReader(rc)
			decoder := xml.NewDecoder(reader)

			if err := decoder.Decode(&pomFile); err != nil {
				return pomFile, err
			}

			return pomFile, nil
		}
	}

	return pomFile, errors.New("unable to parse POM from Jar")
}

// locatorFromJar resolves a locator from a .jar file by inspecting its contents
func locatorFromJar(path string) (module.Locator, error) {
	log.Logger.Debugf("processing locator from Jar: %s", path)

	info, err := jargo.GetJarInfo(path)
	if err == nil {
		// first, attempt to resolve a pomfile from the META-INF directory
		var pomFilePath string
		for _, file := range info.Files {
			if strings.HasPrefix(file, "META-INF") && strings.HasSuffix(file, "pom.xml") && (pomFilePath == "" || len(pomFilePath) > len(file)) {
				pomFilePath = file
			}
		}

		pomFile, err := getPOMFromJar(pomFilePath)
		if err == nil {
			log.Logger.Debugf("resolving locator from pom: %s", pomFilePath)
			return module.Locator{
				Fetcher:  "mvn",
				Project:  pomFile.GroupID + ":" + pomFile.ArtifactID,
				Revision: pomFile.Version,
			}, nil
		} else {
			log.Logger.Debugf("%s", err)
		}

		// failed to decode pom file, fall back to META-INF
		manifest := *info.Manifest
		if manifest["Bundle-SymbolicName"] != "" && manifest["Implementation-Version"] != "" {
			log.Logger.Debugf("resolving locator from META-INF: %s", info.Manifest)
			return module.Locator{
				Fetcher:  "mvn",
				Project:  manifest["Bundle-SymbolicName"], // TODO: identify GroupId
				Revision: manifest["Implementation-Version"],
			}, nil
		}
	}

	// fall back to parsing file name
	re := regexp.MustCompile("(-sources|-javadoc)?.jar$")
	nameParts := strings.Split(re.ReplaceAllString(filepath.Base(path), ""), "-")
	lenNameParts := len(nameParts)

	var parsedProjectName string
	var parsedRevisionName string

	if lenNameParts == 1 {
		parsedProjectName = nameParts[0]
	} else if lenNameParts > 1 {
		parsedProjectName = strings.Join(nameParts[0:lenNameParts-1], "-")
		parsedRevisionName = nameParts[lenNameParts-1]
	}

	if parsedProjectName == "" {
		return module.Locator{}, errors.New("unable to parse jar file")
	}

	return module.Locator{
		Fetcher:  "mvn",
		Project:  parsedProjectName,
		Revision: parsedRevisionName,
	}, nil
}

// IsBuilt always returns true for Ant builds
func (builder *AntBuilder) IsBuilt(m module.Module, allowUnresolved bool) (bool, error) {
	return true, nil
}

// IsModule is not implemented
func (builder *AntBuilder) IsModule(target string) (bool, error) {
	return false, errors.New("IsModule is not implemented for AntBuilder")
}

// DiscoverModules returns a root build.xml if found, and build configs for all sub-projects otherwise
func (builder *AntBuilder) DiscoverModules(dir string) ([]module.Config, error) {
	_, err := os.Stat(filepath.Join(dir, "build.xml"))
	if err == nil {
		// find the root build, as it can invoke tasks sub-builds
		absDir, err := filepath.Abs(dir)
		if err != nil {
			absDir = dir
		}
		artifactName := filepath.Base(absDir)
		return []module.Config{
			{
				Name: artifactName,
				Path: "build.xml",
				Type: "ant",
			},
		}, nil
	}

	// no build.xml in root directory; find and parse all of them
	antFilePaths, err := doublestar.Glob(filepath.Join(dir, "**", "build.xml"))
	if err != nil {
		return nil, err
	}
	moduleConfigs := make([]module.Config, len(antFilePaths))
	for i, path := range antFilePaths {
		artifactName := filepath.Dir(path) // Use the dirname as it's impossible to reliably parse from build.xml
		moduleConfigs[i] = module.Config{
			Name: artifactName,
			Path: path,
			Type: "ant",
		}
	}
	return moduleConfigs, nil
}
