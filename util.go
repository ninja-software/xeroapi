package xeroapi

import (
	"io/ioutil"
	"log"
)

// ReadPrivateKeyFromPath will read a private key from a path, surprisingly enough
func ReadPrivateKeyFromPath(privateKeyFilePath string) string {
	if privateKeyFilePath == "" {
		log.Fatalln("empty keyfile path")
	}

	privateKeyFileContents, err := ioutil.ReadFile(privateKeyFilePath)
	if err != nil {
		log.Fatalln("could not read private key file contents:", err)
	}
	return string(privateKeyFileContents)
}
