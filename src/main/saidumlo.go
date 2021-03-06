package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	configDir = ""
)

/*****************************
 * Config management
 *****************************/
func (saidumlo *SaiDumLo) getConfigDir(configFilePath string) {
	var dirPrefix = "./"
	var result string
	for {
		if _, err := os.Stat(dirPrefix + configFilePath); os.IsNotExist(err) {
			dirPrefix += "../"
			var curDir, _ = filepath.Abs(filepath.Dir(dirPrefix))
			if curDir == "/" {
				logError("Could not find config file '%v'", configFilePath)
				os.Exit(1)
			}
		} else {
			result, err = filepath.Abs(filepath.Dir(dirPrefix + configFilePath))
			checkErr(err)
			break
		}
	}

	saidumlo.ConfigDir = result + "/"
}

// TODO: make less confusing -> proper var names like relativeConfigFilePath..
func (saidumlo *SaiDumLo) parseConfig(configFile string) {
	saidumlo.getConfigDir(configFile)
	saidumlo.ConfigFileName = filepath.Base(configFile)
	saidumlo.Config = Config{}
	configFilePath := saidumlo.ConfigDir + saidumlo.ConfigFileName
	logInfo("Using config %v", configFilePath)
	s, e := ioutil.ReadFile(configFilePath)
	checkErr(e)
	e = yaml.Unmarshal(s, &saidumlo.Config)
	checkErr(e)

	logDebug("%#v", saidumlo.Config)
}

func (saidumlo *SaiDumLo) getDefaultVault() Vault {
	var vault Vault
	var vaultID string
	var alreadyFoundDefault bool

	for k, v := range saidumlo.Config.Vaults {
		if v.Default {
			if alreadyFoundDefault {
				logError("Multiple vaults set as default ('%s' and '%s'), but only one allowed.", vaultID, k)
			} else {
				vault = v
				vaultID = k
				alreadyFoundDefault = true
			}
		}
	}

	return vault
}

func saidumlo(configFile string) SaiDumLo {
	saidumlo := SaiDumLo{}
	saidumlo.parseConfig(configFile)
	return saidumlo
}

/***********
 * Helpers
 ***********/
func createDirIfMissing(path string) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.MkdirAll(path, os.FileMode(0775))
	}
}

func (vault *Vault) walkVaultPath(vaultPath string, localPath string, rootMapping SecretMapping) []SecretMapping {

	mappingList := []SecretMapping{}

	// get all elements in current path
	env := os.Environ()
	env = append(env, fmt.Sprintf("VAULT_ADDR=%s", vault.Address))

	cmd := exec.Command(vault.Bin, "list", "--format=yaml", vaultPath)
	cmd.Env = env
	cmd.Dir = configDir
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	output, commandErr := cmd.Output()
	checkErr(commandErr)

	elements := []string{}
	e := yaml.Unmarshal(output, &elements)
	checkErr(e)

	// go through each element
	for _, element := range elements {
		elementVaultPath := fmt.Sprintf("%s%s", vaultPath, element)
		elementLocalPath := fmt.Sprintf("%s%s", localPath, element)
		if strings.HasSuffix(element, "/") {
			// dir
			mappingList = append(mappingList, vault.walkVaultPath(elementVaultPath, elementLocalPath, rootMapping)...)
		} else {
			// file
			mappingList = append(mappingList, SecretMapping{Local: elementLocalPath, Vault: elementVaultPath, Mod: rootMapping.Mod, Base64: rootMapping.Base64})
		}
	}
	return mappingList
}

func (vault *Vault) generateReadMappingList(secretMapping SecretMapping) []SecretMapping {
	mappingList := []SecretMapping{}

	if strings.HasSuffix(secretMapping.Vault, "*") {
		cleanLocalPathString := strings.Replace(secretMapping.Local, "*", "", -1)
		cleanVaultPathString := strings.Replace(secretMapping.Vault, "*", "", -1)
		mappingList = vault.walkVaultPath(cleanVaultPathString, cleanLocalPathString, secretMapping)
	} else {
		mappingList = append(mappingList, secretMapping)
	}

	return mappingList
}

func (vault *Vault) generateWriteMappingList(secretMapping SecretMapping) []SecretMapping {
	mappingList := []SecretMapping{}

	if strings.HasSuffix(secretMapping.Local, "*") {
		cleanLocalPathString := strings.Replace(secretMapping.Local, "*", "", -1)
		cleanVaultPathString := strings.Replace(secretMapping.Vault, "*", "", -1)
		searchDir := fmt.Sprintf("%s/%s", configDir, filepath.Dir(cleanLocalPathString))
		err := filepath.Walk(searchDir, func(path string, f os.FileInfo, err error) error {
			if f.Mode().IsRegular() {
				relFilePath, rErr := filepath.Rel(searchDir, path)
				checkErr(rErr)
				localMappingPath := fmt.Sprintf("%s%s", cleanLocalPathString, relFilePath)
				vaultMappingPath := fmt.Sprintf("%s%s", cleanVaultPathString, relFilePath)
				mappingList = append(mappingList, SecretMapping{Local: localMappingPath, Vault: vaultMappingPath, Mod: secretMapping.Mod, Base64: secretMapping.Base64})
			}
			return nil
		})
		checkErr(err)
	} else {
		mappingList = append(mappingList, secretMapping)
	}

	return mappingList
}

func getFileContentEncoded(secretMapping SecretMapping) []byte {
	byteData, err := ioutil.ReadFile(fmt.Sprintf("%s/%s", configDir, secretMapping.Local))
	checkErr(err)
	if secretMapping.Base64 {
		byteData = []byte(base64.StdEncoding.EncodeToString(byteData))
	}

	return byteData
}

/***************************
 * Read / Write to Vault
 ***************************/
func (vault *Vault) readSecretMapping(secretMapping SecretMapping, groupMod int) {
	logInfo("%s read -field=value %s > %s/%s", vault.Bin, secretMapping.Vault, configDir, secretMapping.Local)

	// determine file mod
	var mod = 0750 // default
	if secretMapping.Mod != 0 {
		mod = secretMapping.Mod
	} else if groupMod != 0 {
		mod = groupMod
	}
	secretFileMode := os.FileMode(mod)

	createDirIfMissing(fmt.Sprintf("%s/%s", configDir, filepath.Dir(secretMapping.Local)))

	var vaultContentBuffer bytes.Buffer
	vaultContent := []byte{}

	env := os.Environ()
	env = append(env, fmt.Sprintf("VAULT_ADDR=%s", vault.Address))

	cmd := exec.Command(vault.Bin, "read", "-field=value", secretMapping.Vault)
	cmd.Env = env
	cmd.Dir = configDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = &vaultContentBuffer
	cmd.Stderr = os.Stderr
	commandErr := cmd.Run()
	checkErr(commandErr)

	if secretMapping.Base64 {
		data, err := base64.StdEncoding.DecodeString(vaultContentBuffer.String())
		checkErr(err)
		vaultContent = []byte(data)
	} else {
		vaultContent = []byte(vaultContentBuffer.String())
	}

	writeErr := ioutil.WriteFile(fmt.Sprintf("%s/%s", configDir, secretMapping.Local), vaultContent, secretFileMode)
	checkErr(writeErr)
}

func (vault *Vault) writeSecretMapping(secretMapping SecretMapping, leaseTTL string) {
	if leaseTTL != "" {
		leaseTTL = fmt.Sprintf("ttl=%s", leaseTTL)
	}

	logInfo("%s write %s %s value=- (stdin< @%s/%s)", vault.Bin, secretMapping.Vault, leaseTTL, configDir, secretMapping.Local)

	env := os.Environ()
	env = append(env, fmt.Sprintf("VAULT_ADDR=%s", vault.Address))

	cmd := exec.Command(vault.Bin, "write", secretMapping.Vault, leaseTTL, "value=-")
	cmd.Env = env
	cmd.Dir = configDir
	cmd.Stdin = bytes.NewReader(getFileContentEncoded(secretMapping))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	commandErr := cmd.Run()
	checkErr(commandErr)
}

func (vault *Vault) authenticate() {
	if vault.VaultAuth.Method != "" {

		// read credentials from credential file
		args := []string{"auth", fmt.Sprintf("-method=%s", vault.VaultAuth.Method)}
		if file, err := os.Open(fmt.Sprintf("%s/%s", configDir, vault.VaultAuth.CredentialFilePath)); err == nil {

			defer file.Close()

			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				args = append(args, scanner.Text())
			}

			if err = scanner.Err(); err != nil {
				logFatal("%v", err)
			}

		} else {
			logFatal("%v", err)
		}

		// set environment
		env := os.Environ()
		env = append(env, fmt.Sprintf("VAULT_ADDR=%s", vault.Address))

		// execute command
		cmd := exec.Command(vault.Bin, args...)
		cmd.Env = env
		cmd.Dir = configDir
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		commandErr := cmd.Run()
		checkErr(commandErr)
	} else {
		logInfo("No auth method defined - skip auth")
	}
}

/*****************************
 * Command processors
 *****************************/
func (cwsg *CommandWithSecretGroups) processCommandWithSecretGroups(method string) {
	sdl := saidumlo(configFile)
	vault := sdl.getDefaultVault()
	if cwsg.VaultID != "" {
		vault = sdl.Config.Vaults[cwsg.VaultID]
	}
	configDir = sdl.ConfigDir
	logDebug("%+v\n", sdl)

	vault.authenticate()

	var groupsToProcess = cwsg.SecretGroups
	if len(cwsg.SecretGroups) == 0 {
		groupsToProcess = getMapKeys(sdl.Config.SecretGroups)
	}

	for _, secretGroupName := range groupsToProcess {
		var leaseTTL = sdl.Config.SecretGroups[secretGroupName].LeaseTTL
		var groupMod = sdl.Config.SecretGroups[secretGroupName].Mod
		for _, secretMapping := range sdl.Config.SecretGroups[secretGroupName].Mappings {
			if method == writeOperationID {
				// generate SecretMapping list (handle possible wildcards in paths)
				mappingList := vault.generateWriteMappingList(secretMapping)
				for _, mapping := range mappingList {
					vault.writeSecretMapping(mapping, leaseTTL)
				}
			} else if method == readOperationID {
				// generate SecretMapping list (handle possible wildcards in paths)
				mappingList := vault.generateReadMappingList(secretMapping)
				for _, mapping := range mappingList {
					vault.readSecretMapping(mapping, groupMod)
				}
			} else {
				logError("Unknown operation %s", method)
			}
		}
	}
}
