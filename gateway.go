// Copyright (c) 2022 Cloudflare, Inc. All rights reserved.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/chris-wood/ohttp-go"
	"github.com/cisco/go-hpke"
	"google.golang.org/protobuf/proto"
)

const (
	// keying material (seed) should have as many bits of entropy as the bit
	// length of the x25519 secret key
	defaultSeedLength = 32

	// HTTP constants. Fill in your proxy and target here.
	defaultPort     = "8080"
	gatewayEndpoint = "/gateway"
	echoEndpoint    = "/gateway-echo"
	healthEndpoint  = "/health"
	configEndpoint  = "/ohttp-configs"

	// Environment variables
	secretSeedEnvironmentVariable  = "SEED_SECRET_KEY"
	customRequestEncodingType      = "CUSTOM_REQUEST_TYPE"
	customResponseEncodingType     = "CUSTOM_RESPONSE_TYPE"
	certificateEnvironmentVariable = "CERT"
	keyEnvironmentVariable         = "KEY"
)

type gatewayServer struct {
	requestLabel  string
	responseLabel string
	endpoints     map[string]string
	target        *gatewayResource
}

func (s gatewayServer) indexHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s Handling %s\n", r.Method, r.URL.Path)
	fmt.Fprint(w, "OHTTP Gateway\n")
	fmt.Fprint(w, "----------------\n")
	fmt.Fprintf(w, "Config endpoint: https://%s%s\n", r.Host, s.endpoints["Config"])
	fmt.Fprintf(w, "Target endpoint: https://%s%s\n", r.Host, s.endpoints["Target"])
	fmt.Fprintf(w, "   Request content type:  %s\n", s.requestLabel)
	fmt.Fprintf(w, "   Response content type: %s\n", s.responseLabel)
	fmt.Fprintf(w, "Echo endpoint: https://%s%s\n", r.Host, s.endpoints["Echo"])
	fmt.Fprint(w, "----------------\n")
}

func (s gatewayServer) healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s Handling %s\n", r.Method, r.URL.Path)
	fmt.Fprint(w, "ok")
}

func echoHandler(request []byte) ([]byte, error) {
	return request, nil
}

func bhttpHandler(binaryRequest []byte) ([]byte, error) {
	request, err := ohttp.UnmarshalBinaryRequest(binaryRequest)
	if err != nil {
		return nil, err
	}

	client := &http.Client{}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}

	binaryResponse := ohttp.CreateBinaryResponse(response)
	return binaryResponse.Marshal()
}

func protobufHandler(binaryRequest []byte) ([]byte, error) {
	request := &Request{}
	if err := proto.Unmarshal(binaryRequest, request); err != nil {
		return nil, err
	}

	targetRequest, err := protoHTTPToRequest(request)
	if err != nil {
		return nil, err
	}

	client := &http.Client{}
	targetResponse, err := client.Do(targetRequest)
	if err != nil {
		return nil, err
	}

	response, err := responseToProtoHTTP(targetResponse)
	if err != nil {
		return nil, err
	}

	return proto.Marshal(response)
}

func customHandler(request []byte) ([]byte, error) {
	return nil, fmt.Errorf("Not implemented")
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	var seed []byte
	if seedHex := os.Getenv(secretSeedEnvironmentVariable); seedHex != "" {
		log.Printf("Using Secret Key Seed : [%v]", seedHex)
		var err error
		seed, err = hex.DecodeString(seedHex)
		if err != nil {
			panic(err)
		}
	} else {
		seed = make([]byte, defaultSeedLength)
		rand.Read(seed)
	}

	var certFile string
	if certFile = os.Getenv(certificateEnvironmentVariable); certFile == "" {
		certFile = "cert.pem"
	}

	var keyFile string
	enableTLSServe := true
	if keyFile = os.Getenv(keyEnvironmentVariable); keyFile == "" {
		keyFile = "key.pem"
		enableTLSServe = false
	}

	keyID := uint8(0x00)
	config, err := ohttp.NewConfigFromSeed(keyID, hpke.DHKEM_X25519, hpke.KDF_HKDF_SHA256, hpke.AEAD_AESGCM128, seed)
	if err != nil {
		log.Fatalf("Failed to create gateway configuration from seed: %s", err)
	}

	var gateway ohttp.Gateway
	var targetHandler ContentHandler
	requestLabel := os.Getenv(customRequestEncodingType)
	responseLabel := os.Getenv(customResponseEncodingType)
	if requestLabel == "" || responseLabel == "" || requestLabel == responseLabel {
		gateway = ohttp.NewDefaultGateway(config)
		requestLabel = "message/bhttp request"
		responseLabel = "message/bhttp response"
		targetHandler = bhttpHandler
	} else if requestLabel == "message/protohttp request" && responseLabel == "message/protohttp response" {
		gateway = ohttp.NewCustomGateway(config, requestLabel, responseLabel)
		targetHandler = protobufHandler
	} else {
		gateway = ohttp.NewCustomGateway(config, requestLabel, responseLabel)
		targetHandler = customHandler
	}

	handlers := make(map[string]ContentHandler)
	handlers[gatewayEndpoint] = targetHandler // Content-specific handler
	handlers[echoEndpoint] = echoHandler      // Content-agnostic handler
	target := &gatewayResource{
		verbose:  true,
		keyID:    keyID,
		gateway:  gateway,
		handlers: handlers,
	}

	endpoints := make(map[string]string)
	endpoints["Target"] = gatewayEndpoint
	endpoints["Health"] = healthEndpoint
	endpoints["Config"] = configEndpoint

	server := gatewayServer{
		requestLabel:  requestLabel,
		responseLabel: responseLabel,
		endpoints:     endpoints,
		target:        target,
	}

	http.HandleFunc(gatewayEndpoint, server.target.gatewayHandler)
	http.HandleFunc(echoEndpoint, server.target.gatewayHandler)
	http.HandleFunc(healthEndpoint, server.healthCheckHandler)
	http.HandleFunc(configEndpoint, target.configHandler)
	http.HandleFunc("/", server.indexHandler)

	if enableTLSServe {
		log.Printf("Listening on port %v with cert %v and key %v\n", port, certFile, keyFile)
		log.Fatal(http.ListenAndServeTLS(fmt.Sprintf(":%s", port), certFile, keyFile, nil))
	} else {
		log.Printf("Listening on port %v without enabling TLS\n", port)
		log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", port), nil))
	}

}
