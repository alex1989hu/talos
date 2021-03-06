// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package cluster

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	stdruntime "runtime"
	"strconv"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
	talosnet "github.com/talos-systems/net"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/talos-systems/talos/cmd/talosctl/pkg/mgmt/helpers"
	"github.com/talos-systems/talos/internal/pkg/kubeconfig"
	"github.com/talos-systems/talos/pkg/cli"
	"github.com/talos-systems/talos/pkg/cluster/check"
	"github.com/talos-systems/talos/pkg/images"
	clientconfig "github.com/talos-systems/talos/pkg/machinery/client/config"
	"github.com/talos-systems/talos/pkg/machinery/config"
	"github.com/talos-systems/talos/pkg/machinery/config/types/v1alpha1"
	"github.com/talos-systems/talos/pkg/machinery/config/types/v1alpha1/bundle"
	"github.com/talos-systems/talos/pkg/machinery/config/types/v1alpha1/generate"
	"github.com/talos-systems/talos/pkg/machinery/config/types/v1alpha1/machine"
	"github.com/talos-systems/talos/pkg/machinery/constants"
	"github.com/talos-systems/talos/pkg/provision"
	"github.com/talos-systems/talos/pkg/provision/access"
	"github.com/talos-systems/talos/pkg/provision/providers"
	"github.com/talos-systems/talos/pkg/version"
)

var (
	talosconfig             string
	nodeImage               string
	nodeInstallImage        string
	registryMirrors         []string
	registryInsecure        []string
	kubernetesVersion       string
	nodeVmlinuzPath         string
	nodeInitramfsPath       string
	nodeISOPath             string
	nodeDiskImagePath       string
	applyConfigEnabled      bool
	bootloaderEnabled       bool
	uefiEnabled             bool
	configDebug             bool
	networkCIDR             string
	networkMTU              int
	nameservers             []string
	dnsDomain               string
	workers                 int
	masters                 int
	clusterCpus             string
	clusterMemory           int
	clusterDiskSize         int
	clusterDisks            []string
	targetArch              string
	clusterWait             bool
	clusterWaitTimeout      time.Duration
	forceInitNodeAsEndpoint bool
	forceEndpoint           string
	inputDir                string
	cniBinPath              []string
	cniConfDir              string
	cniCacheDir             string
	cniBundleURL            string
	ports                   string
	dockerHostIP            string
	withInitNode            bool
	customCNIUrl            string
	crashdumpOnFailure      bool
	skipKubeconfig          bool
	skipInjectingConfig     bool
)

// createCmd represents the cluster up command.
var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Creates a local docker-based or QEMU-based kubernetes cluster",
	Long:  ``,
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.WithContext(context.Background(), create)
	},
}

//nolint: gocyclo
func create(ctx context.Context) (err error) {
	if masters < 1 {
		return fmt.Errorf("number of masters can't be less than 1")
	}

	nanoCPUs, err := parseCPUShare()
	if err != nil {
		return fmt.Errorf("error parsing --cpus: %s", err)
	}

	memory := int64(clusterMemory) * 1024 * 1024

	// Validate CIDR range and allocate IPs
	fmt.Println("validating CIDR and reserving IPs")

	_, cidr, err := net.ParseCIDR(networkCIDR)
	if err != nil {
		return fmt.Errorf("error validating cidr block: %w", err)
	}

	// Gateway addr at 1st IP in range, ex. 192.168.0.1
	var gatewayIP net.IP

	gatewayIP, err = talosnet.NthIPInNetwork(cidr, 1)
	if err != nil {
		return err
	}

	// Set starting ip at 2nd ip in range, ex: 192.168.0.2
	ips := make([]net.IP, masters+workers)

	for i := range ips {
		ips[i], err = talosnet.NthIPInNetwork(cidr, i+2)
		if err != nil {
			return err
		}
	}

	// Parse nameservers
	nameserverIPs := make([]net.IP, len(nameservers))

	for i := range nameserverIPs {
		nameserverIPs[i] = net.ParseIP(nameservers[i])
		if nameserverIPs[i] == nil {
			return fmt.Errorf("failed parsing nameserver IP %q", nameservers[i])
		}
	}

	provisioner, err := providers.Factory(ctx, provisionerName)
	if err != nil {
		return err
	}

	defer provisioner.Close() //nolint: errcheck

	// Craft cluster and node requests
	request := provision.ClusterRequest{
		Name: clusterName,

		Network: provision.NetworkRequest{
			Name:        clusterName,
			CIDR:        *cidr,
			GatewayAddr: gatewayIP,
			MTU:         networkMTU,
			Nameservers: nameserverIPs,
			CNI: provision.CNIConfig{
				BinPath:  cniBinPath,
				ConfDir:  cniConfDir,
				CacheDir: cniCacheDir,

				BundleURL: cniBundleURL,
			},
		},

		Image:         nodeImage,
		KernelPath:    nodeVmlinuzPath,
		InitramfsPath: nodeInitramfsPath,
		ISOPath:       nodeISOPath,
		DiskImagePath: nodeDiskImagePath,

		SelfExecutable: os.Args[0],
		StateDirectory: stateDir,
	}

	provisionOptions := []provision.Option{
		provision.WithDockerPortsHostIP(dockerHostIP),
		provision.WithBootlader(bootloaderEnabled),
		provision.WithUEFI(uefiEnabled),
		provision.WithTargetArch(targetArch),
	}
	configBundleOpts := []bundle.Option{}

	if ports != "" {
		if provisionerName != "docker" {
			return fmt.Errorf("exposed-ports flag only supported with docker provisioner")
		}

		portList := strings.Split(ports, ",")
		provisionOptions = append(provisionOptions, provision.WithDockerPorts(portList))
	}

	disks, err := getDisks()
	if err != nil {
		return err
	}

	if inputDir != "" {
		configBundleOpts = append(configBundleOpts, bundle.WithExistingConfigs(inputDir))
	} else {
		genOptions := []generate.GenOption{
			generate.WithInstallImage(nodeInstallImage),
			generate.WithDebug(configDebug),
			generate.WithDNSDomain(dnsDomain),
			generate.WithArchitecture(targetArch),
		}

		for _, registryMirror := range registryMirrors {
			components := strings.SplitN(registryMirror, "=", 2)
			if len(components) != 2 {
				return fmt.Errorf("invalid registry mirror spec: %q", registryMirror)
			}

			genOptions = append(genOptions, generate.WithRegistryMirror(components[0], components[1]))
		}

		for _, registryHost := range registryInsecure {
			genOptions = append(genOptions, generate.WithRegistryInsecureSkipVerify(registryHost))
		}

		genOptions = append(genOptions, provisioner.GenOptions(request.Network)...)

		if customCNIUrl != "" {
			genOptions = append(genOptions, generate.WithClusterCNIConfig(&v1alpha1.CNIConfig{
				CNIName: "custom",
				CNIUrls: []string{customCNIUrl},
			}))
		}

		if len(disks) > 1 {
			// convert provision disks to machine disks
			machineDisks := make([]*v1alpha1.MachineDisk, len(disks)-1)
			for i, disk := range disks[1:] {
				machineDisks[i] = &v1alpha1.MachineDisk{
					DeviceName:     provisioner.UserDiskName(i + 1),
					DiskPartitions: disk.Partitions,
				}
			}

			genOptions = append(genOptions, generate.WithUserDisks(machineDisks))
		}

		defaultInternalLB, defaultEndpoint := provisioner.GetLoadBalancers(request.Network)

		if defaultInternalLB == "" {
			// provisioner doesn't provide internal LB, so use first master node
			defaultInternalLB = ips[0].String()
		}

		var endpointList []string

		switch {
		case defaultEndpoint != "":
			if forceEndpoint == "" {
				forceEndpoint = defaultEndpoint
			}

			fallthrough
		case forceEndpoint != "":
			endpointList = []string{forceEndpoint}
			provisionOptions = append(provisionOptions, provision.WithEndpoint(forceEndpoint))
		case forceInitNodeAsEndpoint:
			endpointList = []string{ips[0].String()}
		default:
			// use control plane nodes as endpoints, client-side load-balancing
			for i := 0; i < masters; i++ {
				endpointList = append(endpointList, ips[i].String())
			}
		}

		genOptions = append(genOptions, generate.WithEndpointList(endpointList))
		configBundleOpts = append(configBundleOpts,
			bundle.WithInputOptions(
				&bundle.InputOptions{
					ClusterName: clusterName,
					Endpoint:    fmt.Sprintf("https://%s:%d", defaultInternalLB, constants.DefaultControlPlanePort),
					KubeVersion: kubernetesVersion,
					GenOptions:  genOptions,
				}),
		)
	}

	configBundle, err := bundle.NewConfigBundle(configBundleOpts...)
	if err != nil {
		return err
	}

	if skipInjectingConfig {
		types := []machine.Type{machine.TypeControlPlane, machine.TypeJoin}

		if withInitNode {
			types = append([]machine.Type{machine.TypeInit}, types...)
		}

		if err = configBundle.Write(".", types...); err != nil {
			return err
		}
	}

	// Add talosconfig to provision options so we'll have it to parse there
	provisionOptions = append(provisionOptions, provision.WithTalosConfig(configBundle.TalosConfig()))

	// Create the master nodes.
	for i := 0; i < masters; i++ {
		var cfg config.Provider

		nodeReq := provision.NodeRequest{
			Name:     fmt.Sprintf("%s-master-%d", clusterName, i+1),
			Type:     machine.TypeControlPlane,
			IP:       ips[i],
			Memory:   memory,
			NanoCPUs: nanoCPUs,
			Disks:    disks,
		}

		if i == 0 {
			nodeReq.Ports = []string{"50000:50000/tcp", fmt.Sprintf("%d:%d/tcp", constants.DefaultControlPlanePort, constants.DefaultControlPlanePort)}
		}

		if withInitNode && i == 0 {
			cfg = configBundle.Init()
			nodeReq.Type = machine.TypeInit
		} else {
			cfg = configBundle.ControlPlane()
		}

		if !skipInjectingConfig {
			nodeReq.Config = cfg
		}

		request.Nodes = append(request.Nodes, nodeReq)
	}

	for i := 1; i <= workers; i++ {
		name := fmt.Sprintf("%s-worker-%d", clusterName, i)

		var cfg config.Provider

		if !skipInjectingConfig {
			cfg = configBundle.Join()
		}

		request.Nodes = append(request.Nodes,
			provision.NodeRequest{
				Name:     name,
				Type:     machine.TypeJoin,
				IP:       ips[masters+i-1],
				Memory:   memory,
				NanoCPUs: nanoCPUs,
				Disks:    disks,
				Config:   cfg,
			})
	}

	cluster, err := provisioner.Create(ctx, request, provisionOptions...)
	if err != nil {
		return err
	}

	// Create and save the talosctl configuration file.
	if err = saveConfig(configBundle.TalosConfig()); err != nil {
		return err
	}

	clusterAccess := access.NewAdapter(cluster, provisionOptions...)
	defer clusterAccess.Close() //nolint: errcheck

	if applyConfigEnabled {
		err = clusterAccess.ApplyConfig(ctx, request.Nodes, os.Stdout)
		if err != nil {
			return err
		}
	}

	if err = postCreate(ctx, clusterAccess); err != nil {
		if crashdumpOnFailure {
			provisioner.CrashDump(ctx, cluster, os.Stderr)
		}

		return err
	}

	return showCluster(cluster)
}

func postCreate(ctx context.Context, clusterAccess *access.Adapter) error {
	if !withInitNode {
		if err := clusterAccess.Bootstrap(ctx, os.Stdout); err != nil {
			return fmt.Errorf("bootstrap error: %w", err)
		}
	}

	if !clusterWait {
		return nil
	}

	// Run cluster readiness checks
	checkCtx, checkCtxCancel := context.WithTimeout(ctx, clusterWaitTimeout)
	defer checkCtxCancel()

	if err := check.Wait(checkCtx, clusterAccess, append(check.DefaultClusterChecks(), check.ExtraClusterChecks()...), check.StderrReporter()); err != nil {
		return err
	}

	if !skipKubeconfig {
		if err := mergeKubeconfig(ctx, clusterAccess); err != nil {
			return err
		}
	}

	return nil
}

func saveConfig(talosConfigObj *clientconfig.Config) (err error) {
	c, err := clientconfig.Open(talosconfig)
	if err != nil {
		return err
	}

	renames := c.Merge(talosConfigObj)
	for _, rename := range renames {
		fmt.Printf("renamed talosconfig context %s\n", rename.String())
	}

	return c.Save(talosconfig)
}

func mergeKubeconfig(ctx context.Context, clusterAccess *access.Adapter) error {
	kubeconfigPath, err := kubeconfig.DefaultPath()
	if err != nil {
		return err
	}

	fmt.Printf("\nmerging kubeconfig into %q\n", kubeconfigPath)

	k8sconfig, err := clusterAccess.Kubeconfig(ctx)
	if err != nil {
		return fmt.Errorf("error fetching kubeconfig: %w", err)
	}

	config, err := clientcmd.Load(k8sconfig)
	if err != nil {
		return fmt.Errorf("error parsing kubeconfig: %w", err)
	}

	if clusterAccess.ForceEndpoint != "" {
		for name := range config.Clusters {
			config.Clusters[name].Server = fmt.Sprintf("https://%s:%d", clusterAccess.ForceEndpoint, constants.DefaultControlPlanePort)
		}
	}

	_, err = os.Stat(kubeconfigPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}

		return clientcmd.WriteToFile(*config, kubeconfigPath)
	}

	merger, err := kubeconfig.Load(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("error loading existing kubeconfig: %w", err)
	}

	err = merger.Merge(config, kubeconfig.MergeOptions{
		ActivateContext: true,
		OutputWriter:    os.Stdout,
		ConflictHandler: func(component kubeconfig.ConfigComponent, name string) (kubeconfig.ConflictDecision, error) {
			return kubeconfig.RenameDecision, nil
		},
	})
	if err != nil {
		return fmt.Errorf("error merging kubeconfig: %w", err)
	}

	return merger.Write(kubeconfigPath)
}

func parseCPUShare() (int64, error) {
	cpu, ok := new(big.Rat).SetString(clusterCpus)
	if !ok {
		return 0, fmt.Errorf("failed to parsing as a rational number: %s", clusterCpus)
	}

	nano := cpu.Mul(cpu, big.NewRat(1e9, 1))
	if !nano.IsInt() {
		return 0, errors.New("value is too precise")
	}

	return nano.Num().Int64(), nil
}

func getDisks() ([]*provision.Disk, error) {
	// should have at least a single primary disk
	disks := []*provision.Disk{
		{
			Size: uint64(clusterDiskSize) * 1024 * 1024,
		},
	}

	for _, disk := range clusterDisks {
		var (
			partitions     = strings.Split(disk, ":")
			diskPartitions = make([]*v1alpha1.DiskPartition, len(partitions)/2)
			diskSize       uint64
		)

		if len(partitions)%2 != 0 {
			return nil, fmt.Errorf("failed to parse malformed partition definitions")
		}

		partitionIndex := 0

		for j := 0; j < len(partitions); j += 2 {
			partitionPath := partitions[j]

			if !strings.HasPrefix(partitionPath, "/var") {
				return nil, fmt.Errorf("user disk partitions can only be mounted into /var folder")
			}

			value, e := strconv.ParseInt(partitions[j+1], 10, -1)
			partitionSize := uint64(value)

			if e != nil {
				partitionSize, e = humanize.ParseBytes(partitions[j+1])

				if e != nil {
					return nil, fmt.Errorf("failed to parse partition size")
				}
			}

			diskPartitions[partitionIndex] = &v1alpha1.DiskPartition{
				DiskSize:       v1alpha1.DiskSize(partitionSize),
				DiskMountPoint: partitionPath,
			}
			diskSize += partitionSize
			partitionIndex++
		}

		disks = append(disks, &provision.Disk{
			// add 1 MB to make extra room for GPT
			Size:       diskSize + 1024*1024,
			Partitions: diskPartitions,
		})
	}

	return disks, nil
}

func trimVersion(version string) string {
	// remove anything extra after semantic version core, `v0.3.2-1-abcd` -> `v0.3.2`
	return regexp.MustCompile(`(-\d+(-g[0-9a-f]+)?(-dirty)?)$`).ReplaceAllString(version, "")
}

func init() {
	defaultTalosConfig, err := clientconfig.GetDefaultPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to find default Talos config path: %s", err)
	}

	createCmd.Flags().StringVar(&talosconfig, "talosconfig", defaultTalosConfig, "The path to the Talos configuration file")
	createCmd.Flags().StringVar(&nodeImage, "image", helpers.DefaultImage(images.DefaultTalosImageRepository), "the image to use")
	createCmd.Flags().StringVar(&nodeInstallImage, "install-image", helpers.DefaultImage(images.DefaultInstallerImageRepository), "the installer image to use")
	createCmd.Flags().StringVar(&nodeVmlinuzPath, "vmlinuz-path", helpers.ArtifactPath(constants.KernelAssetWithArch), "the compressed kernel image to use")
	createCmd.Flags().StringVar(&nodeISOPath, "iso-path", "", "the ISO path to use for the initial boot (VM only)")
	createCmd.Flags().StringVar(&nodeInitramfsPath, "initrd-path", helpers.ArtifactPath(constants.InitramfsAssetWithArch), "the uncompressed kernel image to use")
	createCmd.Flags().StringVar(&nodeDiskImagePath, "disk-image-path", "", "disk image to use")
	createCmd.Flags().BoolVar(&applyConfigEnabled, "with-apply-config", false, "enable apply config when the VM is starting in maintenance mode")
	createCmd.Flags().BoolVar(&bootloaderEnabled, "with-bootloader", true, "enable bootloader to load kernel and initramfs from disk image after install")
	createCmd.Flags().BoolVar(&uefiEnabled, "with-uefi", false, "enable UEFI on x86_64 architecture (always enabled for arm64)")
	createCmd.Flags().StringSliceVar(&registryMirrors, "registry-mirror", []string{}, "list of registry mirrors to use in format: <registry host>=<mirror URL>")
	createCmd.Flags().StringSliceVar(&registryInsecure, "registry-insecure-skip-verify", []string{}, "list of registry hostnames to skip TLS verification for")
	createCmd.Flags().BoolVar(&configDebug, "with-debug", false, "enable debug in Talos config to send service logs to the console")
	createCmd.Flags().IntVar(&networkMTU, "mtu", 1500, "MTU of the cluster network")
	createCmd.Flags().StringVar(&networkCIDR, "cidr", "10.5.0.0/24", "CIDR of the cluster network")
	createCmd.Flags().StringSliceVar(&nameservers, "nameservers", []string{"8.8.8.8", "1.1.1.1"}, "list of nameservers to use")
	createCmd.Flags().IntVar(&workers, "workers", 1, "the number of workers to create")
	createCmd.Flags().IntVar(&masters, "masters", 1, "the number of masters to create")
	createCmd.Flags().StringVar(&clusterCpus, "cpus", "2.0", "the share of CPUs as fraction (each container/VM)")
	createCmd.Flags().IntVar(&clusterMemory, "memory", 2048, "the limit on memory usage in MB (each container/VM)")
	createCmd.Flags().IntVar(&clusterDiskSize, "disk", 6*1024, "default limit on disk size in MB (each VM)")
	createCmd.Flags().StringSliceVar(&clusterDisks, "user-disk", []string{}, "list of disks to create for each VM in format: <mount_point1>:<size1>:<mount_point2>:<size2>")
	createCmd.Flags().StringVar(&targetArch, "arch", stdruntime.GOARCH, "cluster architecture")
	createCmd.Flags().BoolVar(&clusterWait, "wait", true, "wait for the cluster to be ready before returning")
	createCmd.Flags().DurationVar(&clusterWaitTimeout, "wait-timeout", 20*time.Minute, "timeout to wait for the cluster to be ready")
	createCmd.Flags().BoolVar(&forceInitNodeAsEndpoint, "init-node-as-endpoint", false, "use init node as endpoint instead of any load balancer endpoint")
	createCmd.Flags().StringVar(&forceEndpoint, "endpoint", "", "use endpoint instead of provider defaults")
	createCmd.Flags().StringVar(&kubernetesVersion, "kubernetes-version", "", fmt.Sprintf("desired kubernetes version to run (default %q)", constants.DefaultKubernetesVersion))
	createCmd.Flags().StringVarP(&inputDir, "input-dir", "i", "", "location of pre-generated config files")
	createCmd.Flags().StringSliceVar(&cniBinPath, "cni-bin-path", []string{filepath.Join(defaultCNIDir, "bin")}, "search path for CNI binaries (VM only)")
	createCmd.Flags().StringVar(&cniConfDir, "cni-conf-dir", filepath.Join(defaultCNIDir, "conf.d"), "CNI config directory path (VM only)")
	createCmd.Flags().StringVar(&cniCacheDir, "cni-cache-dir", filepath.Join(defaultCNIDir, "cache"), "CNI cache directory path (VM only)")
	createCmd.Flags().StringVar(&cniBundleURL, "cni-bundle-url", fmt.Sprintf("https://github.com/talos-systems/talos/releases/download/%s/talosctl-cni-bundle-%s.tar.gz",
		trimVersion(version.Tag), constants.ArchVariable), "URL to download CNI bundle from (VM only)")
	createCmd.Flags().StringVarP(&ports,
		"exposed-ports",
		"p",
		"",
		"Comma-separated list of ports/protocols to expose on init node. Ex -p <hostPort>:<containerPort>/<protocol (tcp or udp)> (Docker provisioner only)",
	)
	createCmd.Flags().StringVar(&dockerHostIP, "docker-host-ip", "0.0.0.0", "Host IP to forward exposed ports to (Docker provisioner only)")
	createCmd.Flags().BoolVar(&withInitNode, "with-init-node", false, "create the cluster with an init node")
	createCmd.Flags().StringVar(&customCNIUrl, "custom-cni-url", "", "install custom CNI from the URL (Talos cluster)")
	createCmd.Flags().StringVar(&dnsDomain, "dns-domain", "cluster.local", "the dns domain to use for cluster")
	createCmd.Flags().BoolVar(&crashdumpOnFailure, "crashdump", false, "print debug crashdump to stderr when cluster startup fails")
	createCmd.Flags().BoolVar(&skipKubeconfig, "skip-kubeconfig", false, "skip merging kubeconfig from the created cluster")
	createCmd.Flags().BoolVar(&skipInjectingConfig, "skip-injecting-config", false, "skip injecting config from embedded metadata server, write config files to current directory")
	Cmd.AddCommand(createCmd)
}
