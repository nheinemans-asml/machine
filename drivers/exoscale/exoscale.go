package exoscale

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/exoscale/egoscale"
	"github.com/rancher/machine/libmachine/drivers"
	rpcdriver "github.com/rancher/machine/libmachine/drivers/rpc"
	"github.com/rancher/machine/libmachine/log"
	"github.com/rancher/machine/libmachine/mcnflag"
	"github.com/rancher/machine/libmachine/mcnutils"
	"github.com/rancher/machine/libmachine/state"
)

// Driver is the struct compatible with github.com/rancher/machine/libmachine/drivers.Driver interface
type Driver struct {
	*drivers.BaseDriver
	URL              string
	APIKey           string `json:"ApiKey"`
	APISecretKey     string `json:"ApiSecretKey"`
	InstanceProfile  string
	DiskSize         int64
	Image            string
	SecurityGroups   []string
	AffinityGroups   []string
	AvailabilityZone string
	SSHKey           string
	KeyPair          string
	Password         string
	PublicKey        string
	UserDataFile     string
	UserData         []byte
	ID               *egoscale.UUID `json:"Id"`
}

const (
	defaultAPIEndpoint       = "https://api.exoscale.ch/compute"
	defaultInstanceProfile   = "Small"
	defaultDiskSize          = 50
	defaultImage             = "Linux Ubuntu 18.04 LTS 64-bit"
	defaultAvailabilityZone  = "CH-DK-2"
	defaultSSHUser           = "root"
	defaultSecurityGroup     = "docker-machine"
	defaultAffinityGroupType = "host anti-affinity"
	defaultCloudInit         = `#cloud-config
manage_etc_hosts: localhost
`
)

// GetCreateFlags registers the flags this driver adds to
// "docker hosts create"
func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.StringFlag{
			EnvVar: "EXOSCALE_ENDPOINT",
			Name:   "exoscale-url",
			Usage:  "exoscale API endpoint",
		},
		mcnflag.StringFlag{
			EnvVar: "EXOSCALE_API_KEY",
			Name:   "exoscale-api-key",
			Usage:  "exoscale API key",
		},
		mcnflag.StringFlag{
			EnvVar: "EXOSCALE_API_SECRET",
			Name:   "exoscale-api-secret-key",
			Usage:  "exoscale API secret key",
		},
		mcnflag.StringFlag{
			EnvVar: "EXOSCALE_INSTANCE_PROFILE",
			Name:   "exoscale-instance-profile",
			Value:  defaultInstanceProfile,
			Usage:  "exoscale instance profile (Small, Medium, Large, ...)",
		},
		mcnflag.IntFlag{
			EnvVar: "EXOSCALE_DISK_SIZE",
			Name:   "exoscale-disk-size",
			Value:  defaultDiskSize,
			Usage:  "exoscale disk size (10, 50, 100, 200, 400)",
		},
		mcnflag.StringFlag{
			EnvVar: "EXOSCALE_IMAGE",
			Name:   "exoscale-image",
			Value:  defaultImage,
			Usage:  "exoscale image template",
		},
		mcnflag.StringSliceFlag{
			EnvVar: "EXOSCALE_SECURITY_GROUP",
			Name:   "exoscale-security-group",
			Value:  []string{defaultSecurityGroup},
			Usage:  "exoscale security group",
		},
		mcnflag.StringFlag{
			EnvVar: "EXOSCALE_AVAILABILITY_ZONE",
			Name:   "exoscale-availability-zone",
			Value:  defaultAvailabilityZone,
			Usage:  "exoscale availability zone",
		},
		mcnflag.StringFlag{
			EnvVar: "EXOSCALE_SSH_USER",
			Name:   "exoscale-ssh-user",
			Value:  "",
			Usage:  "name of the ssh user",
		},
		mcnflag.StringFlag{
			EnvVar: "EXOSCALE_SSH_KEY",
			Name:   "exoscale-ssh-key",
			Value:  "",
			Usage:  "path to the SSH user private key",
		},
		mcnflag.StringFlag{
			EnvVar: "EXOSCALE_USERDATA",
			Name:   "exoscale-userdata",
			Usage:  "path to file with cloud-init user-data",
		},
		mcnflag.StringSliceFlag{
			EnvVar: "EXOSCALE_AFFINITY_GROUP",
			Name:   "exoscale-affinity-group",
			Value:  []string{},
			Usage:  "exoscale affinity group",
		},
	}
}

// NewDriver creates a Driver with the specified machineName and storePath.
func NewDriver(machineName, storePath string) drivers.Driver {
	return &Driver{
		InstanceProfile:  defaultInstanceProfile,
		DiskSize:         defaultDiskSize,
		Image:            defaultImage,
		AvailabilityZone: defaultAvailabilityZone,
		BaseDriver: &drivers.BaseDriver{
			MachineName: machineName,
			StorePath:   storePath,
		},
	}
}

// GetSSHHostname returns the hostname to use with SSH
func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

// GetSSHUsername returns the username to use with SSH
func (d *Driver) GetSSHUsername() string {
	if d.SSHUser == "" {
		name := strings.ToLower(d.Image)

		if strings.Contains(name, "ubuntu") {
			return "ubuntu"
		}
		if strings.Contains(name, "centos") {
			return "centos"
		}
		if strings.Contains(name, "redhat") {
			return "cloud-user"
		}
		if strings.Contains(name, "fedora") {
			return "fedora"
		}
		if strings.Contains(name, "coreos") {
			return "core"
		}
		if strings.Contains(name, "debian") {
			return "debian"
		}
		return defaultSSHUser
	}

	return d.SSHUser
}

// DriverName returns the name of the driver
func (d *Driver) DriverName() string {
	return "exoscale"
}

// UnmarshalJSON loads driver config from JSON.
func (d *Driver) UnmarshalJSON(data []byte) error {
	// Unmarshal driver config into an aliased type to prevent infinite recursion on UnmarshalJSON.
	type targetDriver Driver
	target := targetDriver{}
	if err := json.Unmarshal(data, &target); err != nil {
		return fmt.Errorf("error unmarshalling driver config from JSON: %w", err)
	}

	*d = Driver(target)

	// Make sure to reload values that are subject to change from envvars and os.Args.
	driverOpts := rpcdriver.GetDriverOpts(d.GetCreateFlags(), os.Args)
	if _, ok := driverOpts.Values["exoscale-api-key"]; ok {
		d.APIKey = driverOpts.String("exoscale-api-key")
	}

	if _, ok := driverOpts.Values["exoscale-api-secret-key"]; ok {
		d.APISecretKey = driverOpts.String("exoscale-api-secret-key")
	}

	return nil
}

// SetConfigFromFlags configures the driver with the object that was returned
// by RegisterCreateFlags
func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	d.URL = flags.String("exoscale-url")
	d.APIKey = flags.String("exoscale-api-key")
	d.APISecretKey = flags.String("exoscale-api-secret-key")
	d.InstanceProfile = flags.String("exoscale-instance-profile")
	d.DiskSize = int64(flags.Int("exoscale-disk-size"))
	d.Image = flags.String("exoscale-image")
	d.SecurityGroups = flags.StringSlice("exoscale-security-group")
	d.AffinityGroups = flags.StringSlice("exoscale-affinity-group")
	d.AvailabilityZone = flags.String("exoscale-availability-zone")
	d.SSHUser = flags.String("exoscale-ssh-user")
	d.SSHKey = flags.String("exoscale-ssh-key")
	d.UserDataFile = flags.String("exoscale-userdata")
	d.UserData = []byte(defaultCloudInit)
	d.SetSwarmConfigFromFlags(flags)

	if d.URL == "" {
		d.URL = defaultAPIEndpoint
	}
	if d.APIKey == "" || d.APISecretKey == "" {
		return errors.New("missing an API key (--exoscale-api-key) or API secret key (--exoscale-api-secret-key)")
	}

	return nil
}

// PreCreateCheck allows for pre-create operations to make sure a driver is
// ready for creation
func (d *Driver) PreCreateCheck() error {
	if d.UserDataFile != "" {
		if _, err := os.Stat(d.UserDataFile); os.IsNotExist(err) {
			return fmt.Errorf("user-data file %s could not be found", d.UserDataFile)
		}
	}

	return nil
}

// GetURL returns a Docker compatible host URL for connecting to this host
// e.g tcp://10.1.2.3:2376
func (d *Driver) GetURL() (string, error) {
	if err := drivers.MustBeRunning(d); err != nil {
		return "", err
	}

	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("tcp://%s", net.JoinHostPort(ip, "2376")), nil
}

func (d *Driver) client() *egoscale.Client {
	return egoscale.NewClient(d.URL, d.APIKey, d.APISecretKey)
}

func (d *Driver) virtualMachine() (*egoscale.VirtualMachine, error) {
	cs := d.client()
	virtualMachine := &egoscale.VirtualMachine{
		ID: d.ID,
	}

	if err := cs.GetWithContext(context.TODO(), virtualMachine); err != nil {
		return nil, err
	}

	return virtualMachine, nil
}

// GetState returns a github.com/machine/libmachine/state.State representing the state of the host (running, stopped, etc.)
func (d *Driver) GetState() (state.State, error) {
	vm, err := d.virtualMachine()
	if err != nil {
		return state.Error, err
	}
	switch vm.State {
	case "Starting":
		return state.Starting, nil
	case "Running":
		return state.Running, nil
	case "Stopping":
		return state.Running, nil
	case "Stopped":
		return state.Stopped, nil
	case "Destroyed":
		return state.Stopped, nil
	case "Expunging":
		return state.Stopped, nil
	case "Migrating":
		return state.Paused, nil
	case "Error":
		return state.Error, nil
	case "Unknown":
		return state.Error, nil
	case "Shutdowned":
		return state.Stopped, nil
	}
	return state.None, nil
}

func (d *Driver) createDefaultSecurityGroup(group string) (*egoscale.SecurityGroup, error) {
	cs := d.client()
	resp, err := cs.RequestWithContext(context.TODO(), &egoscale.CreateSecurityGroup{
		Name:        group,
		Description: "created by docker-machine",
	})
	if err != nil {
		return nil, err
	}
	sg := resp.(*egoscale.SecurityGroup)

	cidrList := []egoscale.CIDR{
		*egoscale.MustParseCIDR("0.0.0.0/0"),
		*egoscale.MustParseCIDR("::/0"),
	}

	requests := []egoscale.AuthorizeSecurityGroupIngress{
		{
			SecurityGroupID: sg.ID,
			Description:     "SSH",
			CIDRList:        cidrList,
			Protocol:        "TCP",
			StartPort:       22,
			EndPort:         22,
		},
		{
			SecurityGroupID: sg.ID,
			Description:     "Ping",
			CIDRList:        []egoscale.CIDR{*egoscale.MustParseCIDR("0.0.0.0/0")},
			Protocol:        "ICMP",
			IcmpType:        8,
			IcmpCode:        0,
		},
		{
			SecurityGroupID: sg.ID,
			Description:     "Ping6",
			CIDRList:        []egoscale.CIDR{*egoscale.MustParseCIDR("::/0")},
			Protocol:        "ICMPv6",
			IcmpType:        128,
			IcmpCode:        0,
		},
		{
			SecurityGroupID: sg.ID,
			Description:     "Docker",
			CIDRList:        cidrList,
			Protocol:        "TCP",
			StartPort:       2376,
			EndPort:         2377,
		},
		{
			SecurityGroupID: sg.ID,
			Description:     "Legacy Standalone Swarm",
			CIDRList:        cidrList,
			Protocol:        "TCP",
			StartPort:       3376,
			EndPort:         3377,
		},
		{
			SecurityGroupID: sg.ID,
			Description:     "Communication among nodes",
			Protocol:        "TCP",
			StartPort:       7946,
			EndPort:         7946,
			UserSecurityGroupList: []egoscale.UserSecurityGroup{
				sg.UserSecurityGroup(),
			},
		},
		{
			SecurityGroupID: sg.ID,
			Description:     "Communication among nodes",
			Protocol:        "UDP",
			StartPort:       7946,
			EndPort:         7946,
			UserSecurityGroupList: []egoscale.UserSecurityGroup{
				sg.UserSecurityGroup(),
			},
		},
		{
			SecurityGroupID: sg.ID,
			Description:     "Overlay network traffic",
			Protocol:        "UDP",
			StartPort:       4789,
			EndPort:         4789,
			UserSecurityGroupList: []egoscale.UserSecurityGroup{
				sg.UserSecurityGroup(),
			},
		},
	}

	for _, req := range requests {
		_, err := cs.RequestWithContext(context.TODO(), &req)
		if err != nil {
			return nil, err
		}
	}

	return sg, nil
}

func (d *Driver) createDefaultAffinityGroup(group string) (*egoscale.AffinityGroup, error) {
	cs := d.client()
	resp, err := cs.RequestWithContext(context.TODO(), &egoscale.CreateAffinityGroup{
		Name:        group,
		Type:        defaultAffinityGroupType,
		Description: "created by docker-machine",
	})

	if err != nil {
		return nil, err
	}

	affinityGroup := resp.(*egoscale.AffinityGroup)
	return affinityGroup, nil
}

// Create creates the VM instance acting as the docker host
func (d *Driver) Create() error {
	cloudInit, err := d.getCloudInit()
	if err != nil {
		return err
	}

	log.Infof("Querying exoscale for the requested parameters...")
	client := egoscale.NewClient(d.URL, d.APIKey, d.APISecretKey)

	zones, err := client.ListWithContext(context.TODO(), &egoscale.Zone{
		Name: d.AvailabilityZone,
	})
	if err != nil {
		return err
	}

	if len(zones) != 1 {
		return fmt.Errorf("Availability zone %v doesn't exist",
			d.AvailabilityZone)
	}
	zone := zones[0].(*egoscale.Zone).ID
	log.Debugf("Availability zone %v = %s", d.AvailabilityZone, zone)

	// Image
	template := egoscale.Template{
		IsFeatured: true,
		ZoneID:     zone,
	}

	templates, err := client.ListWithContext(context.TODO(), &template)
	if err != nil {
		return err
	}

	image := strings.ToLower(d.Image)
	re := regexp.MustCompile(`^Linux (?P<name>.+?) (?P<version>[0-9.]+)\b`)

	for _, t := range templates {
		tpl := t.(*egoscale.Template)

		// Keep only 10GiB images
		if tpl.Size>>30 != 10 {
			continue
		}

		fullname := strings.ToLower(tpl.Name)
		if image == fullname {
			template = *tpl
			break
		}

		submatch := re.FindStringSubmatch(tpl.Name)
		if len(submatch) > 0 {
			name := strings.Replace(strings.ToLower(submatch[1]), " ", "-", -1)
			version := submatch[2]
			shortname := fmt.Sprintf("%s-%s", name, version)

			if image == shortname {
				template = *tpl
				break
			}
		}
	}
	if template.ID == nil {
		return fmt.Errorf("Unable to find image %v", d.Image)
	}

	// Reading the username from the template
	if name, ok := template.Details["username"]; ok {
		d.SSHUser = name
	}
	log.Debugf("Image %v(10) = %s (%s)", d.Image, template.ID, d.SSHUser)

	// Profile UUID
	profiles, err := client.ListWithContext(context.TODO(), &egoscale.ServiceOffering{
		Name: d.InstanceProfile,
	})
	if err != nil {
		return err
	}
	if len(profiles) != 1 {
		return fmt.Errorf("Unable to find the %s profile",
			d.InstanceProfile)
	}
	profile := profiles[0].(*egoscale.ServiceOffering).ID
	log.Debugf("Profile %v = %s", d.InstanceProfile, profile)

	// Security groups
	sgs := make([]egoscale.UUID, 0, len(d.SecurityGroups))
	for _, group := range d.SecurityGroups {
		if group == "" {
			continue
		}

		sg := &egoscale.SecurityGroup{Name: group}
		if errGet := client.Get(sg); errGet != nil {
			if _, ok := errGet.(*egoscale.ErrorResponse); !ok {
				return errGet
			}
			log.Infof("Security group %v does not exist. Creating it...", group)
			securityGroup, errCreate := d.createDefaultSecurityGroup(group)
			if errCreate != nil {
				return errCreate
			}
			sg.ID = securityGroup.ID
		}

		log.Debugf("Security group %v = %s", group, sg.ID)
		sgs = append(sgs, *sg.ID)
	}

	// Affinity Groups
	ags := make([]egoscale.UUID, 0, len(d.AffinityGroups))
	for _, group := range d.AffinityGroups {
		if group == "" {
			continue
		}
		ag := &egoscale.AffinityGroup{Name: group}
		if errGet := client.Get(ag); errGet != nil {
			if _, ok := errGet.(*egoscale.ErrorResponse); !ok {
				return err
			}
			log.Infof("Affinity Group %v does not exist, create it", group)
			affinityGroup, errCreate := d.createDefaultAffinityGroup(group)
			if errCreate != nil {
				return errCreate
			}
			ag.ID = affinityGroup.ID
		}
		log.Debugf("Affinity group %v = %s", group, ag.ID)
		ags = append(ags, *ag.ID)
	}

	// SSH key pair
	if d.SSHKey == "" {
		keyPairName := fmt.Sprintf("docker-machine-%s", d.MachineName)
		log.Infof("Generate an SSH keypair...")
		resp, errCreate := client.RequestWithContext(context.TODO(), &egoscale.CreateSSHKeyPair{
			Name: keyPairName,
		})
		if errCreate != nil {
			return fmt.Errorf("SSH Key pair creation failed %s", errCreate)
		}
		keyPair := resp.(*egoscale.SSHKeyPair)
		if errM := os.MkdirAll(filepath.Dir(d.GetSSHKeyPath()), 0750); errM != nil {
			return fmt.Errorf("Cannot create the folder to store the SSH private key. %s", errM)
		}
		if errW := ioutil.WriteFile(d.GetSSHKeyPath(), []byte(keyPair.PrivateKey), 0600); errW != nil {
			return fmt.Errorf("SSH private key could not be written. %s", errW)
		}
		d.KeyPair = keyPairName
	} else {
		log.Infof("Importing SSH key from %s", d.SSHKey)

		sshKey := d.SSHKey
		if strings.HasPrefix(sshKey, "~/") {
			usr, _ := user.Current()
			sshKey = filepath.Join(usr.HomeDir, sshKey[2:])
		} else {
			var errA error
			if sshKey, errA = filepath.Abs(sshKey); errA != nil {
				return errA
			}
		}

		// Sending the SSH public key through the cloud-init config
		pubKey, errR := ioutil.ReadFile(sshKey + ".pub")
		if errR != nil {
			return fmt.Errorf("Cannot read SSH public key %s", errR)
		}

		sshAuthorizedKeys := `
ssh_authorized_keys:
- `
		cloudInit = bytes.Join([][]byte{cloudInit, []byte(sshAuthorizedKeys), pubKey}, []byte(""))

		// Copying the private key into docker-machine
		if errCopy := mcnutils.CopyFile(sshKey, d.GetSSHKeyPath()); errCopy != nil {
			return fmt.Errorf("Unable to copy SSH file: %s", errCopy)
		}
		if errChmod := os.Chmod(d.GetSSHKeyPath(), 0600); errChmod != nil {
			return fmt.Errorf("Unable to set permissions on the SSH file: %s", errChmod)
		}
	}

	log.Infof("Spawn exoscale host...")
	log.Debugf("Using the following cloud-init file:")
	log.Debugf("%s", string(cloudInit))

	// Base64 encode the userdata
	d.UserData = cloudInit
	encodedUserData := base64.StdEncoding.EncodeToString(d.UserData)

	req := &egoscale.DeployVirtualMachine{
		Details:           map[string]string{"ip6": "true"},
		TemplateID:        template.ID,
		ServiceOfferingID: profile,
		UserData:          encodedUserData,
		ZoneID:            zone,
		Name:              d.MachineName,
		KeyPair:           d.KeyPair,
		DisplayName:       d.MachineName,
		RootDiskSize:      d.DiskSize,
		SecurityGroupIDs:  sgs,
		AffinityGroupIDs:  ags,
	}
	log.Infof("Deploying %s...", req.DisplayName)
	resp, err := client.RequestWithContext(context.TODO(), req)
	if err != nil {
		return err
	}

	vm := resp.(*egoscale.VirtualMachine)

	IPAddress := vm.IP()
	if IPAddress != nil {
		d.IPAddress = IPAddress.String()
	}
	d.ID = vm.ID
	log.Infof("IP Address: %v, SSH User: %v", d.IPAddress, d.GetSSHUsername())

	if vm.PasswordEnabled {
		d.Password = vm.Password
	}

	// Destroy the SSH key from CloudStack
	if d.KeyPair != "" {
		if err := drivers.WaitForSSH(d); err != nil {
			return err
		}

		key := &egoscale.SSHKeyPair{
			Name: d.KeyPair,
		}
		if err := client.DeleteWithContext(context.TODO(), key); err != nil {
			return err
		}
		d.KeyPair = ""
	}

	return nil
}

// Start starts the existing VM instance.
func (d *Driver) Start() error {
	cs := d.client()
	_, err := cs.RequestWithContext(context.TODO(), &egoscale.StartVirtualMachine{
		ID: d.ID,
	})

	return err
}

// Stop stops the existing VM instance.
func (d *Driver) Stop() error {
	cs := d.client()
	_, err := cs.RequestWithContext(context.TODO(), &egoscale.StopVirtualMachine{
		ID: d.ID,
	})

	return err
}

// Restart reboots the existing VM instance.
func (d *Driver) Restart() error {
	cs := d.client()
	_, err := cs.RequestWithContext(context.TODO(), &egoscale.RebootVirtualMachine{
		ID: d.ID,
	})

	return err
}

// Kill stops a host forcefully (same as Stop)
func (d *Driver) Kill() error {
	return d.Stop()
}

// Remove destroys the VM instance and the associated SSH key.
func (d *Driver) Remove() error {
	client := d.client()

	// Destroy the SSH key from CloudStack
	if d.KeyPair != "" {
		key := &egoscale.SSHKeyPair{Name: d.KeyPair}
		if err := client.DeleteWithContext(context.TODO(), key); err != nil {
			return err
		}
	}

	// Destroy the virtual machine
	if d.ID != nil {
		vm := &egoscale.VirtualMachine{ID: d.ID}
		if err := client.DeleteWithContext(context.TODO(), vm); err != nil {
			return err
		}
	}

	log.Infof("The Anti-Affinity group and Security group were not removed")

	return nil
}

// Build a cloud-init user data string that will install and run
// docker.
func (d *Driver) getCloudInit() ([]byte, error) {
	var err error
	if d.UserDataFile != "" {
		d.UserData, err = ioutil.ReadFile(d.UserDataFile)
	}

	return d.UserData, err
}
