package main

import (
	"fmt"
	"math"
	"path/filepath"
	"time"
)

const influxDataRPMRepo = `
[influxdata]
name = InfluxData Repository - Stable
baseurl = https://repos.influxdata.com/stable/\$basearch/main
enabled = 1
gpgcheck = 1
gpgkey = https://repos.influxdata.com/influxdb.key
`

type Container struct {
	Name string

	client         LXDClient
	packageManager string
}

// create contianer with given name and image
func (c *Container) Create(image string) error {
	if c.Name == "" {
		return fmt.Errorf("unable to create container: no name given")
	}

	c.client = LXDClient{}
	err := c.client.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to lxd: %v", err)
	}

	err = c.client.Create(c.Name, "images", image)
	if err != nil {
		return fmt.Errorf("failed to create instance: %v", err)
	}

	// at this point the container is created, so on any error during setup
	// we want to delete it as well
	err = c.client.Start(c.Name)
	if err != nil {
		c.Delete()
		return fmt.Errorf("failed to start instance: %v", err)
	}

	if err := c.detectPackageManager(); err != nil {
		c.Delete()
		return err
	}

	if err := c.waitForNetwork(); err != nil {
		c.Delete()
		return err
	}

	if err := c.setupRepo(); err != nil {
		c.Delete()
		return err
	}

	return nil
}

// delete the container
func (c *Container) Delete() {
	_ = c.client.Stop(c.Name)
	_ = c.client.Delete(c.Name)
}

// installs the package from configured repos
func (c *Container) Install(packageName ...string) error {
	var cmd []string
	switch c.packageManager {
	case "apt":
		cmd = append([]string{"apt-get", "install", "--yes"}, packageName...)
	case "yum":
		cmd = append([]string{"yum", "install", "-y"}, packageName...)
	case "dnf":
		cmd = append([]string{"dnf", "install", "-y"}, packageName...)
	case "zypper":
		cmd = append([]string{"zypper", "install", "-y"}, packageName...)
	}

	err := c.client.Exec(c.Name, cmd...)
	if err != nil {
		return err
	}

	return nil
}

func (c *Container) CheckStatus(serviceName string) error {
	// the RPM does not start automatically service on install
	if c.packageManager != "apt" {
		err := c.client.Exec(c.Name, "systemctl", "start", serviceName)
		if err != nil {
			return err
		}
	}

	err := c.client.Exec(c.Name, "systemctl", "status", serviceName)
	if err != nil {
		return err
	}

	return nil
}

func (c *Container) UploadAndInstall(filename string) error {
	basename := filepath.Base(filename)
	destination := fmt.Sprintf("/root/%s", basename)

	if err := c.client.Push(c.Name, filename, destination); err != nil {
		return err
	}

	return c.Install(destination)
}

// Push key and config and update
func (c *Container) configureApt() error {
	err := c.client.Exec(c.Name, "apt-get", "update")
	if err != nil {
		return err
	}

	err = c.Install("ca-certificates", "gpg", "wget")
	if err != nil {
		return err
	}

	err = c.client.Exec(c.Name, "wget", "https://repos.influxdata.com/influxdb.key")
	if err != nil {
		return err
	}

	err = c.client.Exec(
		c.Name,
		"bash",
		"-c",
		"--",
		"echo '23a1c8836f0afc5ed24e0486339d7cc8f6790b83886c4c96995b88a061c5bb5d influxdb.key' | "+
			"sha256sum -c && cat influxdb.key | gpg --dearmor | sudo tee /etc/apt/trusted.gpg.d/influxdb.gpg > /dev/null",
	)
	if err != nil {
		return err
	}

	err = c.client.Exec(
		c.Name,
		"bash",
		"-c",
		"--",
		"echo 'deb [signed-by=/etc/apt/trusted.gpg.d/influxdb.gpg] https://repos.influxdata.com/debian stable main' | "+
			"tee /etc/apt/sources.list.d/influxdata.list",
	)
	if err != nil {
		return err
	}

	err = c.client.Exec(c.Name, "apt-get", "update")
	if err != nil {
		return err
	}

	return nil
}

// Create config and update yum
func (c *Container) configureYum() error {
	err := c.client.Exec(
		c.Name,
		"bash", "-c", "--",
		fmt.Sprintf("echo \"%s\" > /etc/yum.repos.d/influxdata.repo", influxDataRPMRepo),
	)
	if err != nil {
		return err
	}

	// will return a non-zero return code if there are packages to update
	return c.client.Exec(c.Name, "bash", "-c", "yum check-update || true")
}

// Create config and update dnf
func (c *Container) configureDnf() error {
	err := c.client.Exec(
		c.Name,
		"bash", "-c", "--",
		fmt.Sprintf("echo \"%s\" > /etc/yum.repos.d/influxdata.repo", influxDataRPMRepo),
	)
	if err != nil {
		return err
	}

	// will return a non-zero return code if there are packages to update
	return c.client.Exec(c.Name, "bash", "-c", "dnf check-update || true")
}

// Create config and update zypper
func (c *Container) configureZypper() error {
	err := c.client.Exec(
		c.Name,
		"echo", fmt.Sprintf("\"%s\"", influxDataRPMRepo), ">", "/etc/zypp/repos.d/influxdata.repo",
	)
	if err != nil {
		return err
	}

	return c.client.Exec(c.Name, "zypper", "refresh")
}

// Determine if the system uses yum or apt for software
func (c *Container) detectPackageManager() error {
	// Different options required across the distros as apt returns -1 when
	// run with no options. yum is listed last to prefer the newer
	// options first.
	err := c.client.Exec(c.Name, "which", "apt")
	if err == nil {
		c.packageManager = "apt"
		return nil
	}

	err = c.client.Exec(c.Name, "dnf")
	if err == nil {
		c.packageManager = "dnf"
		return nil
	}

	err = c.client.Exec(c.Name, "yum", "version")
	if err == nil {
		c.packageManager = "yum"
		return nil
	}

	return fmt.Errorf("unable to determine package manager")
}

// Configure the system with InfluxData repo
func (c *Container) setupRepo() error {
	if c.packageManager == "apt" {
		if err := c.configureApt(); err != nil {
			return err
		}
	} else if c.packageManager == "yum" {
		if err := c.configureYum(); err != nil {
			return err
		}
	} else if c.packageManager == "zypper" {
		if err := c.configureZypper(); err != nil {
			return err
		}
	} else if c.packageManager == "dnf" {
		if err := c.configureDnf(); err != nil {
			return err
		}
	}

	return nil
}

// Wait for the network to come up on a container
func (c *Container) waitForNetwork() error {
	var exponentialBackoffCeilingSecs int64 = 16

	attempts := 0
	for {
		if err := c.client.Exec(c.Name, "getent", "hosts", "influxdata.com"); err == nil {
			return nil
		}

		// uses exponetnial backoff to try after 1, 2, 4, 8, and 16 seconds
		delaySecs := int64(math.Pow(2, float64(attempts)))
		if delaySecs > exponentialBackoffCeilingSecs {
			break
		}

		time.Sleep(time.Duration(delaySecs) * time.Second)
		attempts++
	}

	return fmt.Errorf("timeout reached waiting for network on container")
}
