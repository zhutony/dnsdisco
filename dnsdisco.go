package dnsdisco

import (
	"net"
	"sort"
	"sync"
	"time"
)

// Discover is the fastest way to find a target using all the default
// parameters. It will send a SRV query in _service._proto.name format and
// return the target (address and port) selected by the RFC 2782 algorithm and
// that passed on the health check (simple connection check).
//
// proto must be "udp" or "tcp", otherwise an UnknownNetworkError error will be
// returned. The library will use the local resolver to send the DNS package.
func Discover(service, proto, name string) (target string, port uint16, err error) {
	discovery := NewDiscovery(service, proto, name)
	if err = discovery.Refresh(); err != nil {
		return
	}

	target, port = discovery.Choose()
	return
}

// Discovery contains all the methods to discover the services and select the
// best one at the moment. The use of interface allows the users to mock this
// library easily for unit tests.
type Discovery interface {
	// Refresh retrieves the servers using the DNS SRV solution. It is possible to
	// change the default behaviour (local resolver with default timeouts) using
	// the SetRetriever method from the Discovery interface.
	Refresh() error

	// RefreshAsync works exactly as Refresh, but is non-blocking and will repeat
	// the action on every interval. To stop the refresh the returned channel must
	// be closed.
	RefreshAsync(time.Duration) chan<- bool

	// Choose will return the best target to use based on a defined load balancer.
	// By default the library choose the server based on the RFC 2782 considering
	// only the online servers. It is possible to change the load balancer
	// behaviour using the SetLoadBalancer method from the Discovery interface. If
	// no good match is found it should return a empty target and a zero port.
	Choose() (target string, port uint16)

	// Errors return all errors found during asynchronous executions. Once this
	// method is called the internal errors buffer is cleared.
	Errors() []error

	// SetRetriever changes how the library retrieves the DNS SRV records.
	SetRetriever(Retriever)

	// SetHealthChecker changes the way the library health check each server.
	SetHealthChecker(HealthChecker)

	// SetLoadBalancer changes how the library selects the best server.
	SetLoadBalancer(LoadBalancer)
}

// discovery stores all the necessary information to discover the services.
type discovery struct {
	// service is the name of the application that the library is looking for.
	service string

	// proto is the protocol used by the application. Could be "udp" or "tcp".
	proto string

	// name is the domain name where the library will look for the SRV records.
	name string

	// retriever is responsible for sending the SRV requests. It is possible to
	// implement this interface to change the retrieve behaviour, that by default
	// queries the local resolver.
	retriever Retriever

	// retrieverLock make it possible to change the retriever algorithm while the
	// library is executing the operations.
	retrieverLock sync.RWMutex

	// healthChecker is responsible for verifying if the target is still on, if
	// not the library can move to the next target. By default the health check
	// only tries a simple connection to the target.
	healthChecker HealthChecker

	// healthCheckerLock make it possible to change the health check algorithm
	// while the library is executing the operations.
	healthCheckerLock sync.RWMutex

	// loadBalancer is responsible for choosing the target that will be used. By
	// default the library choose the target based on the RFC 2782 algorithm.
	loadBalancer LoadBalancer

	// loadBalancerLock make it possible to change the load balancer algorithm
	// while the library is executing the operations.
	loadBalancerLock sync.RWMutex

	// serversLock make it safe to change the servers in the load balancer
	// algorithm.
	serversLock sync.RWMutex

	// errors stores all the error generated by asynchronous methods
	errors []error

	// errorsLock guarantees that the errors list will be go routine safe
	errorsLock sync.Mutex
}

// NewDiscovery builds the default implementation of the Discovery interface. To
// retrieve the servers it will use the net.LookupSRV (local resolver), for
// health check will only perform a simple connection, and the chosen target
// will be selected using the RFC 2782 considering only online servers.
//
// The returned type can be used globally as it is go routine safe. It is
// recommended to keep a global Discovery for each service to minimize the
// number of DNS requests.
func NewDiscovery(service, proto, name string) Discovery {
	return &discovery{
		service:       service,
		name:          name,
		proto:         proto,
		retriever:     NewDefaultRetriever(),
		healthChecker: NewDefaultHealthChecker(),
		loadBalancer:  NewDefaultLoadBalancer(),
	}
}

// Refresh retrieves the servers using the DNS SRV solution. It is possible to
// change the default behaviour (local resolver with default timeouts) using
// the SetRetriever method from the Discovery interface. When the new servers
// are retrieved, a health check is done on each server and the list of servers
// is sort by priority and weight.
func (d *discovery) Refresh() error {
	d.retrieverLock.RLock()
	srvs, err := d.retriever.Retrieve(d.service, d.proto, d.name)
	d.retrieverLock.RUnlock()

	if err != nil {
		return err
	}

	d.serversLock.Lock()
	defer d.serversLock.Unlock()

	var servers []*net.SRV
	for _, srv := range srvs {
		d.healthCheckerLock.RLock()
		ok, err := d.healthChecker.HealthCheck(srv.Target, srv.Port, d.proto)
		d.healthCheckerLock.RUnlock()

		if err != nil {
			d.errorsLock.Lock()
			d.errors = append(d.errors, err)
			d.errorsLock.Unlock()
		}

		if err == nil && ok {
			servers = append(servers, srv)
		}
	}

	// the default retriever already do the sort for us (lookupSRV), but if it's
	// replaced for other algorithm the library needs to ensure that it is
	// ordered, because the default load balancer algorithm depends on that
	byPriorityWeight(servers).sort()

	d.loadBalancerLock.RLock()
	d.loadBalancer.ChangeServers(servers)
	d.loadBalancerLock.RUnlock()
	return nil
}

// RefreshAsync works exactly as Refresh, but is non-blocking and will repeat
// the action on every interval. To stop the refresh the returned channel must
// be closed.
//
// The interval should be at least the TTL of the SRV records, or you will
// retrieve cached information.
func (d *discovery) RefreshAsync(interval time.Duration) chan<- bool {
	finish := make(chan bool)

	go func() {
		for {
			if err := d.Refresh(); err != nil {
				d.errorsLock.Lock()
				d.errors = append(d.errors, err)
				d.errorsLock.Unlock()
			}

			select {
			case <-finish:
				return
			case <-time.Tick(interval):
			}
		}
	}()

	return finish
}

// Choose will return the best target to use based on a defined load balancer.
// By default the library choose the server based on the RFC 2782 considering
// only the online servers. It is possible to change the load balancer behaviour
// using the SetLoadBalancer method from the Discovery interface. If no good
// match is found it should return a empty target and a zero port.
func (d *discovery) Choose() (target string, port uint16) {
	d.serversLock.RLock()
	defer d.serversLock.RUnlock()

	d.loadBalancerLock.RLock()
	target, port = d.loadBalancer.LoadBalance()
	d.loadBalancerLock.RUnlock()

	return
}

// Errors return all errors found during asynchronous executions. Once this
// method is called the internal errors buffer is cleared.
func (d *discovery) Errors() []error {
	d.errorsLock.Lock()
	defer d.errorsLock.Unlock()

	errs := d.errors
	d.errors = nil
	return errs
}

// SetRetriever changes how the library retrieves the DNS SRV records. It is go
// routine safe.
func (d *discovery) SetRetriever(r Retriever) {
	d.retrieverLock.Lock()
	defer d.retrieverLock.Unlock()
	d.retriever = r
}

// SetHealthChecker changes the way the library health check each server. It is
// go routine safe.
func (d *discovery) SetHealthChecker(h HealthChecker) {
	d.healthCheckerLock.Lock()
	defer d.healthCheckerLock.Unlock()
	d.healthChecker = h
}

// SetLoadBalancer changes how the library selects the best server. It is go
// routine safe.
func (d *discovery) SetLoadBalancer(b LoadBalancer) {
	d.loadBalancerLock.Lock()
	defer d.loadBalancerLock.Unlock()
	d.loadBalancer = b
}

// Retriever allows the library user to define a custom DNS retrieve algorithm.
type Retriever interface {
	// Retrieve will send the DNS request and return all SRV records retrieved
	// from the response.
	Retrieve(service, proto, name string) ([]*net.SRV, error)
}

// RetrieverFunc is an easy-to-use implementation of the interface that is
// responsible for sending the DNS SRV requests.
type RetrieverFunc func(service, proto, name string) ([]*net.SRV, error)

// Retrieve will send the DNS request and return all SRV records retrieved from
// the response.
func (r RetrieverFunc) Retrieve(service, proto, name string) ([]*net.SRV, error) {
	return r(service, proto, name)
}

// HealthChecker allows the library user to define a custom health check
// algorithm.
type HealthChecker interface {
	// HealthCheck will analyze the target port/proto to check if it is still
	// capable of receiving requests.
	HealthCheck(target string, port uint16, proto string) (ok bool, err error)
}

// HealthCheckerFunc is an easy-to-use implementation of the interface that is
// responsible for checking if a target is still alive.
type HealthCheckerFunc func(target string, port uint16, proto string) (ok bool, err error)

// HealthCheck will analyze the target port/proto to check if it is still
// capable of receiving requests.
func (h HealthCheckerFunc) HealthCheck(target string, port uint16, proto string) (ok bool, err error) {
	return h(target, port, proto)
}

// LoadBalancer allows the library user to define a custom balance algorithm.
type LoadBalancer interface {
	// ChangeServers will be called anytime that a new set of servers is
	// retrieved.
	ChangeServers(servers []*net.SRV)

	// LoadBalance will choose the best target.
	LoadBalance() (target string, port uint16)
}

// byPriorityWeight was retrieved from file "net/dnsclient.go" of the standard
// library. It is responsible for ordering the servers by priority and weight.
type byPriorityWeight []*net.SRV

// Len returns the size of the slice of servers.
func (servers byPriorityWeight) Len() int { return len(servers) }

// Less returns the server preceding server when analyzing two of them.
func (servers byPriorityWeight) Less(i, j int) bool {
	return servers[i].Priority < servers[j].Priority ||
		(servers[i].Priority == servers[j].Priority && servers[i].Weight < servers[j].Weight)
}

// Swap exchange the servers in the slice.
func (servers byPriorityWeight) Swap(i, j int) {
	servers[i], servers[j] = servers[j], servers[i]
}

// shuffleByWeight shuffles SRV records by weight using the algorithm
// described in RFC 2782.
func (servers byPriorityWeight) shuffleByWeight() {
	sum := 0
	for _, addr := range servers {
		sum += int(addr.Weight)
	}
	for sum > 0 && len(servers) > 1 {
		s := 0
		n := randomSource.Intn(sum)
		for i := range servers {
			s += int(servers[i].Weight)
			if s > n {
				if i > 0 {
					servers[0], servers[i] = servers[i], servers[0]
				}
				break
			}
		}
		sum -= int(servers[0].Weight)
		servers = servers[1:]
	}
}

// sort reorders SRV records as specified in RFC 2782.
func (servers byPriorityWeight) sort() {
	sort.Sort(servers)
	i := 0
	for j := 1; j < len(servers); j++ {
		if servers[i].Priority != servers[j].Priority {
			servers[i:j].shuffleByWeight()
			i = j
		}
	}
	servers[i:].shuffleByWeight()
}
