/*
 * Copyright 2018-present Open Networking Foundation

 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at

 * http://www.apache.org/licenses/LICENSE-2.0

 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"errors"
	"flag"
	"fmt"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/keepalive"
	"k8s.io/api/core/v1"
	"math"
	"os"
	"path"
	"regexp"
	"strconv"
	"time"

	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/opencord/voltha-go/common/log"
	"github.com/opencord/voltha-go/common/version"
	"github.com/opencord/voltha-go/kafka"
	pb "github.com/opencord/voltha-protos/go/afrouter"
	cmn "github.com/opencord/voltha-protos/go/common"
	ic "github.com/opencord/voltha-protos/go/inter_container"
	vpb "github.com/opencord/voltha-protos/go/voltha"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type volthaPod struct {
	name       string
	ipAddr     string
	node       string
	devIds     map[string]struct{}
	cluster    string
	backend    string
	connection string
}

type podTrack struct {
	pod *volthaPod
	dn  bool
}

type Configuration struct {
	DisplayVersionOnly *bool
}

var (
	// if k8s variables are undefined, will attempt to use in-cluster config
	k8sApiServer      = getStrEnv("K8S_API_SERVER", "")
	k8sKubeConfigPath = getStrEnv("K8S_KUBE_CONFIG_PATH", "")

	podNamespace = getStrEnv("POD_NAMESPACE", "voltha")
	podGrpcPort  = uint64(getIntEnv("POD_GRPC_PORT", 0, math.MaxUint16, 50057))

	numRWPods = getIntEnv("NUM_RW_PODS", 1, math.MaxInt32, 6)
	numROPods = getIntEnv("NUM_RO_PODS", 1, math.MaxInt32, 3)

	afrouterApiAddress = getStrEnv("AFROUTER_API_ADDRESS", "localhost:55554")

	afrouterRouterName    = getStrEnv("AFROUTER_ROUTER_NAME", "vcore")
	afrouterRWClusterName = getStrEnv("AFROUTER_RW_CLUSTER_NAME", "vcore")
	afrouterROClusterName = getStrEnv("AFROUTER_RO_CLUSTER_NAME", "ro_vcore")

	kafkaTopic      = getStrEnv("KAFKA_TOPIC", "AffinityRouter")
	kafkaClientType = getStrEnv("KAFKA_CLIENT_TYPE", "sarama")
	kafkaHost       = getStrEnv("KAFKA_HOST", "kafka")
	kafkaPort       = getIntEnv("KAFKA_PORT", 0, math.MaxUint16, 9092)
	kafkaInstanceID = getStrEnv("KAFKA_INSTANCE_ID", "arouterd")
)

func getIntEnv(key string, min, max, defaultValue int) int {
	if val, have := os.LookupEnv(key); have {
		num, err := strconv.Atoi(val)
		if err != nil || !(min <= num && num <= max) {
			panic(fmt.Errorf("%s must be a number in the range [%d, %d]; default: %d", key, min, max, defaultValue))
		}
		return num
	}
	return defaultValue
}

func getStrEnv(key, defaultValue string) string {
	if val, have := os.LookupEnv(key); have {
		return val
	}
	return defaultValue
}

func newKafkaClient(clientType string, host string, port int, instanceID string) (kafka.Client, error) {

	log.Infow("kafka-client-type", log.Fields{"client": clientType})
	switch clientType {
	case "sarama":
		return kafka.NewSaramaClient(
			kafka.Host(host),
			kafka.Port(port),
			kafka.ConsumerType(kafka.GroupCustomer),
			kafka.ProducerReturnOnErrors(true),
			kafka.ProducerReturnOnSuccess(true),
			kafka.ProducerMaxRetries(6),
			kafka.NumPartitions(3),
			kafka.ConsumerGroupName(instanceID),
			kafka.ConsumerGroupPrefix(instanceID),
			kafka.AutoCreateTopic(false),
			kafka.ProducerFlushFrequency(5),
			kafka.ProducerRetryBackoff(time.Millisecond*30)), nil
	}
	return nil, errors.New("unsupported-client-type")
}

func k8sClientSet() *kubernetes.Clientset {
	var config *rest.Config
	if k8sApiServer != "" || k8sKubeConfigPath != "" {
		// use combination of URL & local kube-config file
		c, err := clientcmd.BuildConfigFromFlags(k8sApiServer, k8sKubeConfigPath)
		if err != nil {
			panic(err)
		}
		config = c
	} else {
		// use in-cluster config
		c, err := rest.InClusterConfig()
		if err != nil {
			log.Errorf("Unable to load in-cluster config.  Try setting K8S_API_SERVER and K8S_KUBE_CONFIG_PATH?")
			panic(err)
		}
		config = c
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	return clientset
}

func connect(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	log.Debugf("Trying to connect to %s", addr)
	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithInsecure(),
		grpc.WithBlock(),
		grpc.WithBackoffMaxDelay(time.Second*5),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: time.Second * 10, Timeout: time.Second * 5}))
	if err == nil {
		log.Debugf("Connection succeeded")
	}
	return conn, err
}

func getVolthaPods(cs *kubernetes.Clientset) ([]*volthaPod, []*volthaPod, error) {
	pods, err := cs.CoreV1().Pods(podNamespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}

	// Set up the regular expression to identify the voltha cores
	rwCoreFltr := regexp.MustCompile(`rw-core[0-9]-`)
	roCoreFltr := regexp.MustCompile(`ro-core-`)

	var rwPods, roPods []*volthaPod
items:
	for _, v := range pods.Items {
		// only pods that are actually running should be considered
		if v.Status.Phase == v1.PodRunning {
			for _, condition := range v.Status.Conditions {
				if condition.Status != v1.ConditionTrue {
					continue items
				}
			}

			if rwCoreFltr.MatchString(v.Name) {
				log.Debugf("Namespace: %s, PodName: %s, PodIP: %s, Host: %s\n", v.Namespace, v.Name, v.Status.PodIP, v.Spec.NodeName)
				rwPods = append(rwPods, &volthaPod{name: v.Name, ipAddr: v.Status.PodIP, node: v.Spec.NodeName, devIds: make(map[string]struct{}), backend: "", connection: ""})
			} else if roCoreFltr.MatchString(v.Name) {
				log.Debugf("Namespace: %s, PodName: %s, PodIP: %s, Host: %s\n", v.Namespace, v.Name, v.Status.PodIP, v.Spec.NodeName)
				roPods = append(roPods, &volthaPod{name: v.Name, ipAddr: v.Status.PodIP, node: v.Spec.NodeName, devIds: make(map[string]struct{}), backend: "", connection: ""})
			}
		}
	}
	return rwPods, roPods, nil
}

func reconcilePodDeviceIds(ctx context.Context, pod *volthaPod, ids map[string]struct{}) bool {
	ctxTimeout, _ := context.WithTimeout(ctx, time.Second*5)
	conn, err := connect(ctxTimeout, fmt.Sprintf("%s:%d", pod.ipAddr, podGrpcPort))
	if err != nil {
		log.Debugf("Could not query devices from %s, could not connect", pod.name)
		return false
	}
	defer conn.Close()

	var idList cmn.IDs
	for k := range ids {
		idList.Items = append(idList.Items, &cmn.ID{Id: k})
	}

	client := vpb.NewVolthaServiceClient(conn)
	_, err = client.ReconcileDevices(ctx, &idList)
	if err != nil {
		log.Error(err)
		return false
	}

	return true
}

func queryPodDeviceIds(ctx context.Context, pod *volthaPod) map[string]struct{} {
	var rtrn = make(map[string]struct{})
	// Open a connection to the pod
	ctxTimeout, _ := context.WithTimeout(ctx, time.Second*5)
	conn, err := connect(ctxTimeout, fmt.Sprintf("%s:%d", pod.ipAddr, podGrpcPort))
	if err != nil {
		log.Debugf("Could not query devices from %s, could not connect", pod.name)
		return rtrn
	}
	defer conn.Close()
	client := vpb.NewVolthaServiceClient(conn)
	devs, err := client.ListDeviceIds(ctx, &empty.Empty{})
	if err != nil {
		log.Error(err)
		return rtrn
	}
	for _, dv := range devs.Items {
		rtrn[dv.Id] = struct{}{}
	}

	return rtrn
}

func queryDeviceIds(ctx context.Context, pods []*volthaPod) {
	for pk := range pods {
		// Keep the old Id list if a new list is not returned
		if idList := queryPodDeviceIds(ctx, pods[pk]); len(idList) != 0 {
			pods[pk].devIds = idList
		}
	}
}

func allEmpty(pods []*volthaPod) bool {
	for k := range pods {
		if len(pods[k].devIds) != 0 {
			return false
		}
	}
	return true
}

func rmPod(pods []*volthaPod, idx int) []*volthaPod {
	return append(pods[:idx], pods[idx+1:]...)
}

func groupIntersectingPods1(pods []*volthaPod, podCt int) ([][]*volthaPod, []*volthaPod) {
	var rtrn [][]*volthaPod
	var out []*volthaPod

	for {
		if len(pods) == 0 {
			break
		}
		if len(pods[0].devIds) == 0 { // Ignore pods with no devices
			////log.Debugf("%s empty pod", pd[k].pod.name)
			out = append(out, pods[0])
			pods = rmPod(pods, 0)
			continue
		}
		// Start a pod group with this pod
		var grp []*volthaPod
		grp = append(grp, pods[0])
		pods = rmPod(pods, 0)
		//log.Debugf("Creating new group %s", pd[k].pod.name)
		// Find the peer pod based on device overlap
		// It's ok if one isn't found, an empty one will be used instead
		for k := range pods {
			if len(pods[k].devIds) == 0 { // Skip pods with no devices
				//log.Debugf("%s empty pod", pd[k1].pod.name)
				continue
			}
			if intersect(grp[0].devIds, pods[k].devIds) {
				//log.Debugf("intersection found %s:%s", pd[k].pod.name, pd[k1].pod.name)
				if grp[0].node == pods[k].node {
					// This should never happen
					log.Errorf("pods %s and %s intersect and are on the same server!! Not pairing",
						grp[0].name, pods[k].name)
					continue
				}
				grp = append(grp, pods[k])
				pods = rmPod(pods, k)
				break

			}
		}
		rtrn = append(rtrn, grp)
		//log.Debugf("Added group %s", grp[0].name)
		// Check if the number of groups = half the pods, if so all groups are started.
		if len(rtrn) == podCt>>1 {
			// Append any remaining pods to out
			out = append(out, pods[0:]...)
			break
		}
	}
	return rtrn, out
}

func unallocPodCount(pd []*podTrack) int {
	var rtrn int = 0
	for _, v := range pd {
		if !v.dn {
			rtrn++
		}
	}
	return rtrn
}

func sameNode(pod *volthaPod, grps [][]*volthaPod) bool {
	for _, v := range grps {
		if v[0].node == pod.node {
			return true
		}
		if len(v) == 2 && v[1].node == pod.node {
			return true
		}
	}
	return false
}

func startRemainingGroups1(grps [][]*volthaPod, pods []*volthaPod, podCt int) ([][]*volthaPod, []*volthaPod) {
	var grp []*volthaPod

	for k := range pods {
		if sameNode(pods[k], grps) {
			continue
		}
		grp = []*volthaPod{}
		grp = append(grp, pods[k])
		pods = rmPod(pods, k)
		grps = append(grps, grp)
		if len(grps) == podCt>>1 {
			break
		}
	}
	return grps, pods
}

func hasSingleSecondNode(grp []*volthaPod) bool {
	var servers = make(map[string]struct{})
	for k := range grp {
		if k == 0 {
			continue // Ignore the first item
		}
		servers[grp[k].node] = struct{}{}
	}
	if len(servers) == 1 {
		return true
	}
	return false
}

func addNode(grps [][]*volthaPod, idx *volthaPod, item *volthaPod) [][]*volthaPod {
	for k := range grps {
		if grps[k][0].name == idx.name {
			grps[k] = append(grps[k], item)
			return grps
		}
	}
	// TODO: Error checking required here.
	return grps
}

func removeNode(grps [][]*volthaPod, item *volthaPod) [][]*volthaPod {
	for k := range grps {
		for k1 := range grps[k] {
			if grps[k][k1].name == item.name {
				grps[k] = append(grps[k][:k1], grps[k][k1+1:]...)
				break
			}
		}
	}
	return grps
}

func groupRemainingPods1(grps [][]*volthaPod, pods []*volthaPod) [][]*volthaPod {
	var lgrps [][]*volthaPod
	// All groups must be started when this function is called.
	// Copy incomplete groups
	for k := range grps {
		if len(grps[k]) != 2 {
			lgrps = append(lgrps, grps[k])
		}
	}

	// Add all pairing candidates to each started group.
	for k := range pods {
		for k2 := range lgrps {
			if lgrps[k2][0].node != pods[k].node {
				lgrps[k2] = append(lgrps[k2], pods[k])
			}
		}
	}

	//TODO: If any member of lgrps doesn't have at least 2
	// nodes something is wrong. Check for that here

	for {
		for { // Address groups with only a single server choice
			var ssn bool = false

			for k := range lgrps {
				// Now if any of the groups only have a single
				// node as the choice for the second member
				// address that one first.
				if hasSingleSecondNode(lgrps[k]) {
					ssn = true
					// Add this pairing to the groups
					grps = addNode(grps, lgrps[k][0], lgrps[k][1])
					// Since this node is now used, remove it from all
					// remaining tenative groups
					lgrps = removeNode(lgrps, lgrps[k][1])
					// Now remove this group completely since
					// it's been addressed
					lgrps = append(lgrps[:k], lgrps[k+1:]...)
					break
				}
			}
			if !ssn {
				break
			}
		}
		// Now address one of the remaining groups
		if len(lgrps) == 0 {
			break // Nothing left to do, exit the loop
		}
		grps = addNode(grps, lgrps[0][0], lgrps[0][1])
		lgrps = removeNode(lgrps, lgrps[0][1])
		lgrps = append(lgrps[:0], lgrps[1:]...)
	}
	return grps
}

func groupPods1(pods []*volthaPod) [][]*volthaPod {
	var rtrn [][]*volthaPod
	var podCt int = len(pods)

	rtrn, pods = groupIntersectingPods1(pods, podCt)
	// There are several outcomes here
	// 1) All pods have been paired and we're done
	// 2) Some un-allocated pods remain
	// 2.a) All groups have been started
	// 2.b) Not all groups have been started
	if len(pods) == 0 {
		return rtrn
	} else if len(rtrn) == podCt>>1 { // All groupings started
		// Allocate the remaining (presumably empty) pods to the started groups
		return groupRemainingPods1(rtrn, pods)
	} else { // Some groupings started
		// Start empty groups with remaining pods
		// each grouping is on a different server then
		// allocate remaining pods.
		rtrn, pods = startRemainingGroups1(rtrn, pods, podCt)
		return groupRemainingPods1(rtrn, pods)
	}
}

func intersect(d1 map[string]struct{}, d2 map[string]struct{}) bool {
	for k := range d1 {
		if _, ok := d2[k]; ok {
			return true
		}
	}
	return false
}

func setConnection(ctx context.Context, client pb.ConfigurationClient, cluster string, backend string, connection string, addr string, port uint64) {
	log.Debugf("Configuring backend %s : connection %s in cluster %s\n\n",
		backend, connection, cluster)
	cnf := &pb.Conn{Server: "grpc_command", Cluster: cluster, Backend: backend,
		Connection: connection, Addr: addr,
		Port: port}
	if res, err := client.SetConnection(ctx, cnf); err != nil {
		log.Debugf("failed SetConnection RPC call: %s", err)
	} else {
		log.Debugf("Result: %v", res)
	}
}

func setAffinity(ctx context.Context, client pb.ConfigurationClient, ids map[string]struct{}, backend string) {
	log.Debugf("Configuring backend %s : affinities \n", backend)
	aff := &pb.Affinity{Router: afrouterRouterName, Route: "dev_manager", Cluster: afrouterRWClusterName, Backend: backend}
	for k := range ids {
		log.Debugf("Setting affinity for id %s", k)
		aff.Id = k
		if res, err := client.SetAffinity(ctx, aff); err != nil {
			log.Debugf("failed affinity RPC call: %s", err)
		} else {
			log.Debugf("Result: %v", res)
		}
	}
}

func getBackendForCore(coreId string, coreGroups [][]*volthaPod) string {
	for _, v := range coreGroups {
		for _, v2 := range v {
			if v2.name == coreId {
				return v2.backend
			}
		}
	}
	log.Errorf("No backend found for core %s\n", coreId)
	return ""
}

func monitorDiscovery(ctx context.Context,
	client pb.ConfigurationClient,
	ch <-chan *ic.InterContainerMessage,
	coreGroups [][]*volthaPod,
	doneCh chan<- struct{}) {
	defer close(doneCh)

	var id = make(map[string]struct{})

	select {
	case <-ctx.Done():
	case msg := <-ch:
		log.Debugf("Received a device discovery notification")
		device := &ic.DeviceDiscovered{}
		if err := ptypes.UnmarshalAny(msg.Body, device); err != nil {
			log.Errorf("Could not unmarshal received notification %v", msg)
		} else {
			// Set the affinity of the discovered device.
			if be := getBackendForCore(device.Id, coreGroups); be != "" {
				id[device.Id] = struct{}{}
				setAffinity(ctx, client, id, be)
			} else {
				log.Error("Cant use an empty string as a backend name")
			}
		}
		break
	}
}

func startDiscoveryMonitor(ctx context.Context,
	client pb.ConfigurationClient,
	coreGroups [][]*volthaPod) (<-chan struct{}, error) {
	doneCh := make(chan struct{})
	var ch <-chan *ic.InterContainerMessage
	// Connect to kafka for discovery events
	topic := &kafka.Topic{Name: kafkaTopic}
	kc, err := newKafkaClient(kafkaClientType, kafkaHost, kafkaPort, kafkaInstanceID)
	kc.Start()
	defer kc.Stop()

	if ch, err = kc.Subscribe(topic); err != nil {
		log.Errorf("Could not subscribe to the '%s' channel, discovery disabled", kafkaTopic)
		close(doneCh)
		return doneCh, err
	}

	go monitorDiscovery(ctx, client, ch, coreGroups, doneCh)
	return doneCh, nil
}

// Determines which items in core groups
// have changed based on the list provided
// and returns a coreGroup with only the changed
// items and a pod list with the new items
func getAddrDiffs(coreGroups [][]*volthaPod, rwPods []*volthaPod) ([][]*volthaPod, []*volthaPod) {
	var nList []*volthaPod
	var rtrn = make([][]*volthaPod, numRWPods>>1)
	var ipAddrs = make(map[string]struct{})

	log.Debug("Get addr diffs")

	// Start with an empty array
	for k := range rtrn {
		rtrn[k] = make([]*volthaPod, 2)
	}

	// Build a list with only the new items
	for _, v := range rwPods {
		if !hasIpAddr(coreGroups, v.ipAddr) {
			nList = append(nList, v)
		}
		ipAddrs[v.ipAddr] = struct{}{} // for the search below
	}

	// Now build the coreGroups with only the changed items
	for k1, v1 := range coreGroups {
		for k2, v2 := range v1 {
			if _, ok := ipAddrs[v2.ipAddr]; !ok {
				rtrn[k1][k2] = v2
			}
		}
	}
	return rtrn, nList
}

// Figure out where best to put the new pods
// in the coreGroup array based on the old
// pods being replaced. The criteria is that
// the new pod be on the same server as the
// old pod was.
func reconcileAddrDiffs(coreGroupDiffs [][]*volthaPod, rwPodDiffs []*volthaPod) [][]*volthaPod {
	var srvrs map[string][]*volthaPod = make(map[string][]*volthaPod)

	log.Debug("Reconciling diffs")
	log.Debug("Building server list")
	for _, v := range rwPodDiffs {
		log.Debugf("Adding %v to the server list", *v)
		srvrs[v.node] = append(srvrs[v.node], v)
	}

	for k1, v1 := range coreGroupDiffs {
		log.Debugf("k1:%v, v1:%v", k1, v1)
		for k2, v2 := range v1 {
			log.Debugf("k2:%v, v2:%v", k2, v2)
			if v2 == nil { // Nothing to do here
				continue
			}
			if _, ok := srvrs[v2.node]; ok {
				coreGroupDiffs[k1][k2] = srvrs[v2.node][0]
				if len(srvrs[v2.node]) > 1 { // remove one entry from the list
					srvrs[v2.node] = append(srvrs[v2.node][:0], srvrs[v2.node][1:]...)
				} else { // Delete the endtry from the map
					delete(srvrs, v2.node)
				}
			} else {
				log.Error("This should never happen, node appears to have changed names")
				// attempt to limp along by keeping this old entry
			}
		}
	}

	return coreGroupDiffs
}

func applyAddrDiffs(ctx context.Context, client pb.ConfigurationClient, coreList interface{}, nPods []*volthaPod) {
	log.Debug("Applying diffs")
	switch cores := coreList.(type) {
	case [][]*volthaPod:
		newEntries := reconcileAddrDiffs(getAddrDiffs(cores, nPods))

		// Now replace the information in coreGropus with the new
		// entries and then reconcile the device ids on the core
		// that's in the new entry with the device ids of it's
		// active-active peer.
		for k1, v1 := range cores {
			for k2, v2 := range v1 {
				if newEntries[k1][k2] != nil {
					// TODO: Missing is the case where bothe the primary
					// and the secondary core crash and come back.
					// Pull the device ids from the active-active peer
					ids := queryPodDeviceIds(ctx, cores[k1][k2^1])
					if len(ids) != 0 {
						if !reconcilePodDeviceIds(ctx, newEntries[k1][k2], ids) {
							log.Errorf("Attempt to reconcile ids on pod %v failed", newEntries[k1][k2])
						}
					}
					// Send the affininty router new connection information
					setConnection(ctx, client, afrouterRWClusterName, v2.backend, v2.connection, newEntries[k1][k2].ipAddr, podGrpcPort)
					// Copy the new entry information over
					cores[k1][k2].ipAddr = newEntries[k1][k2].ipAddr
					cores[k1][k2].name = newEntries[k1][k2].name
					cores[k1][k2].devIds = ids
				}
			}
		}
	case []*volthaPod:
		var mia []*volthaPod
		var found bool
		// TODO: Break this using functions to simplify
		// reading of the code.
		// Find the core(s) that have changed addresses
		for k1, v1 := range cores {
			found = false
			for _, v2 := range nPods {
				if v1.ipAddr == v2.ipAddr {
					found = true
					break
				}
			}
			if !found {
				mia = append(mia, cores[k1])
			}
		}
		// Now plug in the new addresses and set the connection
		for _, v1 := range nPods {
			found = false
			for _, v2 := range cores {
				if v1.ipAddr == v2.ipAddr {
					found = true
					break
				}
			}
			if found {
				continue
			}
			mia[0].ipAddr = v1.ipAddr
			mia[0].name = v1.name
			setConnection(ctx, client, afrouterROClusterName, mia[0].backend, mia[0].connection, v1.ipAddr, podGrpcPort)
			// Now get rid of the mia entry just processed
			mia = append(mia[:0], mia[1:]...)
		}
	default:
		log.Error("Internal: Unexpected type in call to applyAddrDiffs")
	}
}

func updateDeviceIds(coreGroups [][]*volthaPod, rwPods []*volthaPod) {
	// Convenience
	var byName = make(map[string]*volthaPod)
	for _, v := range rwPods {
		byName[v.name] = v
	}

	for k1, v1 := range coreGroups {
		for k2, v2 := range v1 {
			if pod, have := byName[v2.name]; have {
				coreGroups[k1][k2].devIds = pod.devIds
			}
		}
	}
}

func startCoreMonitor(ctx context.Context,
	client pb.ConfigurationClient,
	clientset *kubernetes.Clientset,
	coreGroups [][]*volthaPod,
	oRoPods []*volthaPod) {
	// Now that initial allocation has been completed, monitor the pods
	// for IP changes
	// The main loop needs to do the following:
	// 1) Periodically query the pods and filter out
	//    the vcore ones
	// 2) Validate that the pods running are the same
	//    as the previous check
	// 3) Validate that the IP addresses are the same
	//    as the last check.
	// If the pod name(s) ha(s/ve) changed then remove
	// the unused pod names and add in the new pod names
	// maintaining the cluster/backend information.
	// If an IP address has changed (which shouldn't
	// happen unless a pod is re-started) it should get
	// caught by the pod name change.
loop:
	for {
		select {
		case <-ctx.Done():
			// if we're done, exit
			break loop
		case <-time.After(10 * time.Second): //wait a while
		}

		// Get the rw core list from k8s
		rwPods, roPods, err := getVolthaPods(clientset)
		if err != nil {
			log.Error(err)
			continue
		}

		// If we didn't get 2n+1 pods then wait since
		// something is down and will hopefully come
		// back up at some point.
		if len(rwPods) != numRWPods {
			log.Debug("One or more RW pod(s) are offline, will wait and retry")
			continue
		}

		queryDeviceIds(ctx, rwPods)
		updateDeviceIds(coreGroups, rwPods)

		// We have all pods, check if any IP addresses
		// have changed.
		for _, v := range rwPods {
			if !hasIpAddr(coreGroups, v.ipAddr) {
				log.Debug("Address has changed...")
				applyAddrDiffs(ctx, client, coreGroups, rwPods)
				break
			}
		}

		if len(roPods) != numROPods {
			log.Debug("One or more RO pod(s) are offline, will wait and retry")
			continue
		}
		for _, v := range roPods {
			if !hasIpAddr(oRoPods, v.ipAddr) {
				applyAddrDiffs(ctx, client, oRoPods, roPods)
				break
			}
		}
	}
}

func hasIpAddr(coreList interface{}, ipAddr string) bool {
	switch cores := coreList.(type) {
	case []*volthaPod:
		for _, v := range cores {
			if v.ipAddr == ipAddr {
				return true
			}
		}
	case [][]*volthaPod:
		for _, v1 := range cores {
			for _, v2 := range v1 {
				if v2.ipAddr == ipAddr {
					return true
				}
			}
		}
	default:
		log.Error("Internal: Unexpected type in call to hasIpAddr")
	}
	return false
}

// endOnClose cancels the context when the connection closes
func connectionActiveContext(conn *grpc.ClientConn) context.Context {
	ctx, disconnected := context.WithCancel(context.Background())
	go func() {
		for state := conn.GetState(); state != connectivity.TransientFailure && state != connectivity.Shutdown; state = conn.GetState() {
			if !conn.WaitForStateChange(context.Background(), state) {
				break
			}
		}
		log.Infof("Connection to afrouter lost")
		disconnected()
	}()
	return ctx
}

func main() {
	config := &Configuration{}
	cmdParse := flag.NewFlagSet(path.Base(os.Args[0]), flag.ContinueOnError)
	config.DisplayVersionOnly = cmdParse.Bool("version", false, "Print version information and exit")

	if err := cmdParse.Parse(os.Args[1:]); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	if *config.DisplayVersionOnly {
		fmt.Println("VOLTHA API Server (afrouterd)")
		fmt.Println(version.VersionInfo.String("  "))
		return
	}

	// Set up logging
	if _, err := log.SetDefaultLogger(log.JSON, 0, nil); err != nil {
		log.With(log.Fields{"error": err}).Fatal("Cannot setup logging")
	}

	// Set up kubernetes api
	clientset := k8sClientSet()

	for {
		// Connect to the affinity router
		conn, err := connect(context.Background(), afrouterApiAddress) // This is a sidecar container so communicating over localhost
		if err != nil {
			panic(err)
		}

		// monitor the connection status, end context if connection is lost
		ctx := connectionActiveContext(conn)

		// set up the client
		client := pb.NewConfigurationClient(conn)

		// determine config & repopulate the afrouter
		generateAndMaintainConfiguration(ctx, client, clientset)

		conn.Close()
	}
}

// generateAndMaintainConfiguration does the pod-reconciliation work,
// it only returns once all sub-processes have completed
func generateAndMaintainConfiguration(ctx context.Context, client pb.ConfigurationClient, clientset *kubernetes.Clientset) {
	// Get the voltha rw-/ro-core pods
	var rwPods, roPods []*volthaPod
	for {
		var err error
		if rwPods, roPods, err = getVolthaPods(clientset); err != nil {
			log.Error(err)
			return
		}

		if len(rwPods) == numRWPods && len(roPods) == numROPods {
			break
		}

		log.Debug("One or more RW/RO pod(s) are offline, will wait and retry")
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second * 5):
			// retry
		}
	}

	// Fetch the devices held by each running core
	queryDeviceIds(ctx, rwPods)

	// For debugging... comment out l8r
	for _, v := range rwPods {
		log.Debugf("Pod list %v", *v)
	}

	coreGroups := groupPods1(rwPods)

	// Assign the groupings to the the backends and connections
	for k, coresInGroup := range coreGroups {
		for k1 := range coresInGroup {
			coreGroups[k][k1].cluster = afrouterRWClusterName
			coreGroups[k][k1].backend = afrouterRWClusterName + strconv.Itoa(k+1)
			coreGroups[k][k1].connection = afrouterRWClusterName + strconv.Itoa(k+1) + strconv.Itoa(k1+1)
		}
	}
	log.Info("Core grouping completed")

	// TODO: Debugging code, comment out for production
	for k, v := range coreGroups {
		for k2, v2 := range v {
			log.Debugf("Core group %d,%d: %v", k, k2, v2)
		}
	}
	log.Info("Setting affinities")
	// Now set the affinities for exising devices in the cores
	for _, v := range coreGroups {
		setAffinity(ctx, client, v[0].devIds, v[0].backend)
		setAffinity(ctx, client, v[1].devIds, v[1].backend)
	}
	log.Info("Setting connections")
	// Configure the backeds based on the calculated core groups
	for _, v := range coreGroups {
		setConnection(ctx, client, afrouterRWClusterName, v[0].backend, v[0].connection, v[0].ipAddr, podGrpcPort)
		setConnection(ctx, client, afrouterRWClusterName, v[1].backend, v[1].connection, v[1].ipAddr, podGrpcPort)
	}

	// Process the read only pods
	for k, v := range roPods {
		log.Debugf("Processing ro_pod %v", v)
		vN := afrouterROClusterName + strconv.Itoa(k+1)
		log.Debugf("Setting connection %s, %s, %s", vN, vN+"1", v.ipAddr)
		roPods[k].cluster = afrouterROClusterName
		roPods[k].backend = vN
		roPods[k].connection = vN + "1"
		setConnection(ctx, client, afrouterROClusterName, v.backend, v.connection, v.ipAddr, podGrpcPort)
	}

	log.Info("Starting discovery monitoring")
	doneCh, _ := startDiscoveryMonitor(ctx, client, coreGroups)

	log.Info("Starting core monitoring")
	startCoreMonitor(ctx, client, clientset, coreGroups, roPods)

	//ensure the discovery monitor to quit
	<-doneCh
}
