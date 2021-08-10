module integration_test

go 1.16

require (
	github.com/cenkalti/backoff v2.2.1+incompatible
	github.com/containerd/containerd v1.5.4 // indirect
	github.com/docker/docker v20.10.7+incompatible
	github.com/docker/go-connections v0.4.0 // indirect
	github.com/go-git/go-git/v5 v5.4.2
	github.com/golang/protobuf v1.4.3
	github.com/hashicorp/go-multierror v1.1.1
	github.com/openconfig/gnmi v0.0.0-20210707145734-c69a5df04b53
	github.com/sirupsen/logrus v1.8.1
	google.golang.org/grpc v1.39.0 // indirect
	// keysight.com/otgclient v0.0.0-00010101000000-000000000000
  github.com/open-traffic-generator/models/ v0.3.6
)

// replace keysight.com/otgclient => ./libs/otgclient