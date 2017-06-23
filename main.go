//go:generate go-bindata openapi/openapi/fixtures.json openapi/openapi/spec2.json

package main

import (
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Fixtures struct {
	Resources map[ResourceID]interface{} `json:"resources"`
}

type HTTPVerb string

type JSONSchema struct {
	Enum       []string               `json:"enum"`
	Items      *JSONSchema            `json:"items"`
	Properties map[string]*JSONSchema `json:"properties"`
	Type       []string               `json:"type"`

	// Ref is populated if this JSON Schema is actually a JSON reference, and
	// it defines the location of the actual schema definition.
	Ref string `json:"$ref"`

	XResourceID string `json:"x-resourceId"`
}

type OpenAPIParameter struct {
	Description string      `json:"description"`
	In          string      `json:"in"`
	Name        string      `json:"name"`
	Required    bool        `json:"required"`
	Schema      *JSONSchema `json:"schema"`
}

type OpenAPIMethod struct {
	Description string                                `json:"description"`
	OperationID string                                `json:"operation_id"`
	Parameters  []OpenAPIParameter                    `json:"parameters"`
	Responses   map[OpenAPIStatusCode]OpenAPIResponse `json:"responses"`
}

type OpenAPIPath string

type OpenAPIResponse struct {
	Description string      `json:"description"`
	Schema      *JSONSchema `json:"schema"`
}

type OpenAPISpec struct {
	Definitions map[string]*JSONSchema                      `json:"definitions"`
	Paths       map[OpenAPIPath]map[HTTPVerb]*OpenAPIMethod `json:"paths"`
}

type OpenAPIStatusCode string

type ResourceID string

type StubServerRoute struct {
	pattern *regexp.Regexp
	method  *OpenAPIMethod
}

type StubServer struct {
	fixtures *Fixtures
	routes   map[HTTPVerb][]StubServerRoute
	spec     *OpenAPISpec
}

func (s *StubServer) routeRequest(r *http.Request) *OpenAPIMethod {
	verbRoutes := s.routes[HTTPVerb(r.Method)]
	for _, route := range verbRoutes {
		if route.pattern.MatchString(r.URL.Path) {
			return route.method
		}
	}
	return nil
}

func (s *StubServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	log.Printf("Request: %v %v", r.Method, r.URL.Path)
	start := time.Now()

	method := s.routeRequest(r)
	if method == nil {
		writeResponse(w, start, http.StatusNotFound, nil)
		return
	}

	response, ok := method.Responses["200"]
	if !ok {
		log.Printf("Couldn't find 200 response in spec")
		writeResponse(w, start, http.StatusInternalServerError, nil)
		return
	}

	if verbose {
		log.Printf("Response schema: %+v", response.Schema)
	}

	generator := DataGenerator{s.spec.Definitions, s.fixtures}
	data, err := generator.Generate(response.Schema, r.URL.Path)
	if err != nil {
		log.Printf("Couldn't generate response: %v", err)
		writeResponse(w, start, http.StatusInternalServerError, nil)
		return
	}
	writeResponse(w, start, http.StatusOK, data)
}

func (s *StubServer) initializeRouter() {
	var numEndpoints int
	var numPaths int

	s.routes = make(map[HTTPVerb][]StubServerRoute)

	for path, verbs := range s.spec.Paths {
		numPaths++

		pathPattern := compilePath(path)

		if verbose {
			log.Printf("Compiled path: %v", pathPattern.String())
		}

		for verb, method := range verbs {
			numEndpoints++

			route := StubServerRoute{
				pattern: pathPattern,
				method:  method,
			}

			// net/http will always give us verbs in uppercase, so build our
			// routing table this way too
			verb = HTTPVerb(strings.ToUpper(string(verb)))

			s.routes[verb] = append(s.routes[verb], route)
		}
	}

	log.Printf("Routing to %v path(s) and %v endpoint(s)",
		numPaths, numEndpoints)
}

// ---

var pathParameterPattern = regexp.MustCompile(`\{(\w+)\}`)

func compilePath(path OpenAPIPath) *regexp.Regexp {
	pattern := `\A`
	parts := strings.Split(string(path), "/")

	for _, part := range parts {
		if part == "" {
			continue
		}

		submatches := pathParameterPattern.FindAllStringSubmatch(part, -1)
		if submatches == nil {
			pattern += `/` + part
		} else {
			pattern += `/(?P<` + submatches[0][1] + `>\w+)`
		}
	}

	return regexp.MustCompile(pattern + `\z`)
}

func writeResponse(w http.ResponseWriter, start time.Time, status int, data interface{}) {
	if data == nil {
		data = []byte(http.StatusText(status))
	}

	encodedData, err := json.Marshal(&data)
	if err != nil {
		log.Printf("Error serializing response: %v", err)
		writeResponse(w, start, http.StatusInternalServerError, nil)
		return
	}

	w.WriteHeader(status)
	_, err = w.Write(encodedData)
	if err != nil {
		log.Printf("Error writing to client: %v", err)
	}
	log.Printf("Response: elapsed=%v status=%v", time.Now().Sub(start), status)
	if verbose {
		log.Printf("Response body: %v", encodedData)
	}
}

// ---

const defaultPort = 6065

// verbose tracks whether the program is operating in verbose mode
var verbose bool

func main() {
	var port int
	var unix string
	flag.IntVar(&port, "port", 0, "Port to listen on")
	flag.StringVar(&unix, "unix", "", "Unix socket to listen on")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose mode")
	flag.Parse()

	if unix != "" && port != 0 {
		flag.Usage()
		log.Fatalf("Specify only one of -port or -unix")
	}

	// Load the spec information from go-bindata
	data, err := Asset("openapi/openapi/spec2.json")
	if err != nil {
		log.Fatalf("Error loading spec: %v", err)
	}

	var spec OpenAPISpec
	err = json.Unmarshal(data, &spec)
	if err != nil {
		log.Fatalf("Error decoding spec: %v", err)
	}

	// And do the same for fixtures
	data, err = Asset("openapi/openapi/fixtures.json")
	if err != nil {
		log.Fatalf("Error loading fixtures: %v", err)
	}

	var fixtures Fixtures
	err = json.Unmarshal(data, &fixtures)
	if err != nil {
		log.Fatalf("Error decoding spec: %v", err)
	}

	stub := StubServer{fixtures: &fixtures, spec: &spec}
	stub.initializeRouter()

	var listener net.Listener
	if unix != "" {
		listener, err = net.Listen("unix", unix)
		log.Printf("Listening on unix socket %v", unix)
	} else {
		if port == 0 {
			port = defaultPort
		}
		listener, err = net.Listen("tcp", ":"+strconv.Itoa(port))
		log.Printf("Listening on port %v", port)
	}
	if err != nil {
		log.Fatalf("Error listening on socket: %v", err)
	}

	http.HandleFunc("/", stub.handleRequest)
	server := http.Server{}
	server.Serve(listener)
}
