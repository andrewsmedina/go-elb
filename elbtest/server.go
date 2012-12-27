// Package elbtest implements a fake ELB provider with the capability of
// inducing errors on any given operation, and retrospectively determining what
// operations have been carried out.
package elbtest

import (
    "fmt"
    "encoding/xml"
	"launchpad.net/goamz/elb"
	"net"
    "net/http"
	"sync"
)

// Server implements an ELB simulator for use in testing.
type Server struct {
	url       string
	listener  net.Listener
	mutex     sync.Mutex
	reqId     int
    lbs       []string
    instances []string
    instCount int
}

// Starts and returns a new server
func NewServer() (*Server, error) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return nil, fmt.Errorf("cannot listen on localhost: %v", err)
	}
	srv := &Server{
		listener: l,
		url:      "http://" + l.Addr().String(),
	}
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		srv.serveHTTP(w, req)
	}))
	return srv, nil
}

// Quit closes down the server.
func (srv *Server) Quit() {
	srv.listener.Close()
}

// URL returns the URL of the server.
func (srv *Server) URL() string {
	return srv.url
}

type xmlErrors struct {
	XMLName string `xml:"ErrorResponse"`
	Error   elb.Error
}

func (srv *Server) error(w http.ResponseWriter, err *elb.Error) {
	w.WriteHeader(err.StatusCode)
	xmlErr := xmlErrors{Error: *err}
	if e := xml.NewEncoder(w).Encode(xmlErr); e != nil {
		panic(e)
	}
}

func (srv *Server) serveHTTP(w http.ResponseWriter, req *http.Request) {
	req.ParseForm()
	srv.mutex.Lock()
	defer srv.mutex.Unlock()
	f := actions[req.Form.Get("Action")]
	if f == nil {
		srv.error(w, &elb.Error{
			StatusCode: 400,
			Code:       "InvalidParameterValue",
			Message:    "Unrecognized Action",
		})
	}
    reqId := fmt.Sprintf("req%0X", srv.reqId)
    srv.reqId++
    if resp, err := f(srv, w, req, reqId); err == nil {
        if err := xml.NewEncoder(w).Encode(resp); err != nil {
            panic(err)
        }
    } else {
        switch err.(type) {
        case *elb.Error:
            srv.error(w, err.(*elb.Error))
        default:
            panic(err)
        }
    }
}

func (srv *Server) createLoadBalancer(w http.ResponseWriter, req *http.Request, reqId string) (interface{}, error) {
    composition := map[string]string{
        "AvailabilityZones.member.1": "Subnets.member.1",
    }
    if err := srv.validateComposition(req, composition); err != nil {
        return nil, err
    }
    required := []string{
        "Listeners.member.1.InstancePort",
        "Listeners.member.1.InstanceProtocol",
        "Listeners.member.1.Protocol",
        "Listeners.member.1.LoadBalancerPort",
        "LoadBalancerName",
    }
    if err := srv.validate(req, required); err != nil {
        return nil, err
    }
	path := req.FormValue("Path")
	if path == "" {
		path = "/"
	}
    srv.lbs = append(srv.lbs, req.FormValue("LoadBalancerName"))
    return elb.CreateLoadBalancerResp{
        DNSName: fmt.Sprintf("%s-some-aws-stuff.us-east-1.elb.amazonaws.com", req.FormValue("LoadBalancerName")),
    }, nil
}

func (srv *Server) deleteLoadBalancer(w http.ResponseWriter, req *http.Request, reqId string) (interface{}, error) {
    if err := srv.validate(req, []string{"LoadBalancerName"}); err != nil {
        return nil, err
    }
    for i, lb := range srv.lbs {
        if lb == req.FormValue("LoadBalancerName") {
            srv.lbs[i], srv.lbs = srv.lbs[len(srv.lbs)-1], srv.lbs[:len(srv.lbs)-1]
            break
        }
    }
    return elb.SimpleResp{RequestId: reqId}, nil
}

func (srv *Server) registerInstancesWithLoadBalancer(w http.ResponseWriter, req *http.Request, reqId string) (interface{}, error) {
    required := []string{"LoadBalancerName", "Instances.member.1.InstanceId"}
    if err := srv.validate(req, required); err != nil {
        return nil, err
    }
    if err := srv.lbExists(req.FormValue("LoadBalancerName")); err != nil {
        return nil, err
    }
    instIds := []string{}
    i := 1
    instId := req.FormValue(fmt.Sprintf("Instances.member.%d.InstanceId", i))
    for instId != "" {
        if err := srv.instanceExists(instId); err != nil {
            return nil, err
        }
        instIds = append(instIds, instId)
        i++
        instId = req.FormValue(fmt.Sprintf("Instances.member.%d.InstanceId", i))
    }
    return elb.RegisterInstancesResp{InstanceIds: instIds}, nil
}

func (srv *Server) deregisterInstancesFromLoadBalancer(w http.ResponseWriter, req *http.Request, reqId string) (interface{}, error) {
    required := []string{"LoadBalancerName", "Instances.member.1.InstanceId"}
    if err := srv.validate(req, required); err != nil {
        return nil, err
    }
    if err := srv.lbExists(req.FormValue("LoadBalancerName")); err != nil {
        return nil, err
    }
    i := 1
    instId := req.FormValue(fmt.Sprintf("Instances.member.%d.InstanceId", i))
    for instId != "" {
        if err := srv.instanceExists(instId); err != nil {
            return nil, err
        }
        i++
        instId = req.FormValue(fmt.Sprintf("Instances.member.%d.InstanceId", i))
    }
    return elb.SimpleResp{RequestId: reqId}, nil
}

func (srv *Server) instanceExists(id string) error {
    for _, instId := range srv.instances {
        if instId == id {
            return nil
        }
    }
    return &elb.Error{
        StatusCode: 400,
        Code:       "InvalidInstance",
        Message:    fmt.Sprintf("InvalidInstance found in [%s]. Invalid id: \"%s\"", id, id),
    }
}

func (srv *Server) lbExists(name string) error {
    index := -1
    for i, lb := range srv.lbs {
        if lb == name {
            index = i
            break
        }
    }
    if index < 0 {
        return &elb.Error{
            StatusCode: 400,
            Code:       "LoadBalancerNotFound",
            Message:    fmt.Sprintf("There is no ACTIVE Load Balancer named '%s'", name),
        }
    }
    return nil
}

func (srv *Server) validate(req *http.Request, required []string) error {
    for _, field := range required {
        if req.FormValue(field) == "" {
			return &elb.Error{
				StatusCode: 400,
				Code:       "ValidationError",
				Message:    fmt.Sprintf("%s is required.", field),
			}
        }
    }
    return nil
}

// Validates the composition of the fields.
//
// Some fields cannot be together in the same request, such as AvailabilityZones and Subnets.
// A sample map with the above requirement would be
//    c := map[string]string{
//        "AvailabilityZones.member.1": "Subnets.member.1",
//    }
//
// The server also requires that at least one of those fields are specified.
func (srv *Server) validateComposition(req *http.Request, composition map[string]string) error {
    for k, v := range composition {
        if req.FormValue(k) != "" && req.FormValue(v) != "" {
			return &elb.Error{
				StatusCode: 400,
				Code:       "ValidationError",
				Message:    fmt.Sprintf("Only one of %s or %s may be specified", k, v),
			}
        }
        if req.FormValue(k) == "" && req.FormValue(v) == "" {
			return &elb.Error{
				StatusCode: 400,
				Code:       "ValidationError",
				Message:    fmt.Sprintf("Either %s or %s must be specified", k, v),
			}
        }
    }
    return nil
}

// Creates a fake instance in the server
func (srv *Server) NewInstance() string {
    srv.instCount++
    instId := fmt.Sprintf("i-%d", srv.instCount)
    srv.instances = append(srv.instances, instId)
    return instId
}

// Removes a fake instance from the server
//
// If no instance is found it does nothing
func (srv *Server) RemoveInstance(instId string) {
    for i, id := range srv.instances {
        if id == instId {
            srv.instances[i], srv.instances = srv.instances[len(srv.instances)-1], srv.instances[:len(srv.instances)-1]
        }
    }
}

// Creates a fake load balancer in the fake server
func (srv *Server) NewLoadBalancer(name string) {
    srv.lbs = append(srv.lbs, name)
}

// Removes a fake load balancer from the fake server
func (srv *Server) RemoveLoadBalancer(name string) {
    for i, lb := range srv.lbs {
        if lb == name {
            srv.lbs[i], srv.lbs = srv.lbs[len(srv.lbs)-1], srv.lbs[:len(srv.lbs)-1]
        }
    }
}

var actions = map[string]func(*Server, http.ResponseWriter, *http.Request, string) (interface{}, error){
	"CreateLoadBalancer":                  (*Server).createLoadBalancer,
	"DeleteLoadBalancer":                  (*Server).deleteLoadBalancer,
	"RegisterInstancesWithLoadBalancer":   (*Server).registerInstancesWithLoadBalancer,
	"DeregisterInstancesFromLoadBalancer": (*Server).deregisterInstancesFromLoadBalancer,
}
