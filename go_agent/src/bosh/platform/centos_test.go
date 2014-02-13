package platform_test

import (
	. "bosh/platform"
	fakecd "bosh/platform/cdutil/fakes"
	boshcmd "bosh/platform/commands"
	boshdisk "bosh/platform/disk"
	fakedisk "bosh/platform/disk/fakes"
	fakestats "bosh/platform/stats/fakes"
	boshvitals "bosh/platform/vitals"
	boshsettings "bosh/settings"
	boshdirs "bosh/settings/directories"
	fakesys "bosh/system/fakes"
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"os"
	"path/filepath"
	"time"
)

const CENTOS_EXPECTED_ETC_HOSTS = `127.0.0.1 localhost foobar.local

# The following lines are desirable for IPv6 capable hosts
::1 localhost ip6-localhost ip6-loopback foobar.local
fe00::0 ip6-localnet
ff00::0 ip6-mcastprefix
ff02::1 ip6-allnodes
ff02::2 ip6-allrouters
ff02::3 ip6-allhosts
`

const CENTOS_EXPECTED_DHCP_CONFIG = `# Generated by bosh-agent

option rfc3442-classless-static-routes code 121 = array of unsigned integer 8;

send host-name "<hostname>";

request subnet-mask, broadcast-address, time-offset, routers,
	domain-name, domain-name-servers, domain-search, host-name,
	netbios-name-servers, netbios-scope, interface-mtu,
	rfc3442-classless-static-routes, ntp-servers;

prepend domain-name-servers zz.zz.zz.zz;
prepend domain-name-servers yy.yy.yy.yy;
prepend domain-name-servers xx.xx.xx.xx;
`

const CENTOS_EXPECTED_IFCFG = `DEVICE=eth0
BOOTPROTO=static
IPADDR=192.168.195.6
NETMASK=255.255.255.0
BROADCAST=192.168.195.255
GATEWAY=192.168.195.1
ONBOOT=yes`

const CENTOS_EXPECTED_RESOLV_CONF = `nameserver 10.80.130.1
nameserver 10.80.130.2
`

const CENTOS_EXPECTED_ETC_LOGROTATE = `# Generated by bosh-agent

fake-base-path/data/sys/log/*.log fake-base-path/data/sys/log/*/*.log fake-base-path/data/sys/log/*/*/*.log {
  missingok
  rotate 7
  compress
  delaycompress
  copytruncate
  size=fake-size
}
`

func init() {
	Describe("Testing with Ginkgo", func() {
		var (
			collector     *fakestats.FakeStatsCollector
			fs            *fakesys.FakeFileSystem
			cmdRunner     *fakesys.FakeCmdRunner
			diskManager   fakedisk.FakeDiskManager
			dirProvider   boshdirs.DirectoriesProvider
			platform      Platform
			cdutil        *fakecd.FakeCdUtil
			compressor    boshcmd.Compressor
			copier        boshcmd.Copier
			vitalsService boshvitals.Service
		)

		BeforeEach(func() {
			collector = &fakestats.FakeStatsCollector{}
			fs = &fakesys.FakeFileSystem{}
			cmdRunner = &fakesys.FakeCmdRunner{}
			diskManager = fakedisk.NewFakeDiskManager(cmdRunner)
			dirProvider = boshdirs.NewDirectoriesProvider("/fake-dir")
			cdutil = fakecd.NewFakeCdUtil()
			compressor = boshcmd.NewTarballCompressor(cmdRunner, fs)
			copier = boshcmd.NewCpCopier(cmdRunner, fs)
			vitalsService = boshvitals.NewService(collector, dirProvider)
		})

		JustBeforeEach(func() {
			platform = NewCentosPlatform(
				collector,
				fs,
				cmdRunner,
				diskManager,
				compressor,
				copier,
				vitalsService,
				dirProvider,
				cdutil,
				1*time.Millisecond,
				1*time.Millisecond,
			)
		})

		It("centos setup runtime configuration", func() {
			err := platform.SetupRuntimeConfiguration()
			assert.NoError(GinkgoT(), err)

			assert.Equal(GinkgoT(), 1, len(cmdRunner.RunCommands))
			assert.Equal(GinkgoT(), []string{"bosh-agent-rc"}, cmdRunner.RunCommands[0])
		})

		It("centos create user", func() {
			expectedUseradd := []string{
				"useradd",
				"-m",
				"-b", "/some/path/to/home",
				"-s", "/bin/bash",
				"-p", "bar-pwd",
				"foo-user",
			}

			password := "bar-pwd"
			err := platform.CreateUser("foo-user", password, "/some/path/to/home")
			assert.NoError(GinkgoT(), err)

			basePathStat := fs.GetFileTestStat("/some/path/to/home")
			assert.Equal(GinkgoT(), fakesys.FakeFileTypeDir, basePathStat.FileType)
			assert.Equal(GinkgoT(), os.FileMode(0755), basePathStat.FileMode)

			assert.Equal(GinkgoT(), 1, len(cmdRunner.RunCommands))
			assert.Equal(GinkgoT(), expectedUseradd, cmdRunner.RunCommands[0])
		})

		It("centos create user with an empty password", func() {
			expectedUseradd := []string{
				"useradd",
				"-m",
				"-b", "/some/path/to/home",
				"-s", "/bin/bash",
				"foo-user",
			}

			password := ""

			err := platform.CreateUser("foo-user", password, "/some/path/to/home")
			assert.NoError(GinkgoT(), err)

			basePathStat := fs.GetFileTestStat("/some/path/to/home")
			assert.Equal(GinkgoT(), fakesys.FakeFileTypeDir, basePathStat.FileType)
			assert.Equal(GinkgoT(), os.FileMode(0755), basePathStat.FileMode)

			assert.Equal(GinkgoT(), 1, len(cmdRunner.RunCommands))
			assert.Equal(GinkgoT(), expectedUseradd, cmdRunner.RunCommands[0])
		})

		It("centos add user to groups", func() {
			err := platform.AddUserToGroups("foo-user", []string{"group1", "group2", "group3"})
			assert.NoError(GinkgoT(), err)

			assert.Equal(GinkgoT(), 1, len(cmdRunner.RunCommands))

			usermod := []string{"usermod", "-G", "group1,group2,group3", "foo-user"}
			assert.Equal(GinkgoT(), usermod, cmdRunner.RunCommands[0])
		})

		It("centos delete users with prefix and regex", func() {
			passwdFile := fmt.Sprintf(`%sfoo:...
%sbar:...
foo:...
bar:...
foobar:...
%sfoobar:...`,
				boshsettings.EPHEMERAL_USER_PREFIX, boshsettings.EPHEMERAL_USER_PREFIX, boshsettings.EPHEMERAL_USER_PREFIX,
			)

			fs.WriteToFile("/etc/passwd", passwdFile)

			err := platform.DeleteEphemeralUsersMatching("bar$")
			assert.NoError(GinkgoT(), err)
			assert.Equal(GinkgoT(), 2, len(cmdRunner.RunCommands))
			assert.Equal(GinkgoT(), []string{"userdel", "-r", "bosh_bar"}, cmdRunner.RunCommands[0])
			assert.Equal(GinkgoT(), []string{"userdel", "-r", "bosh_foobar"}, cmdRunner.RunCommands[1])
		})

		It("centos setup ssh", func() {
			fs.HomeDirHomePath = "/some/home/dir"

			platform.SetupSsh("some public key", "vcap")

			sshDirPath := "/some/home/dir/.ssh"
			sshDirStat := fs.GetFileTestStat(sshDirPath)

			assert.Equal(GinkgoT(), fs.HomeDirUsername, "vcap")

			assert.NotNil(GinkgoT(), sshDirStat)
			assert.Equal(GinkgoT(), fakesys.FakeFileTypeDir, sshDirStat.FileType)
			assert.Equal(GinkgoT(), sshDirStat.FileMode, os.FileMode(0700))
			assert.Equal(GinkgoT(), sshDirStat.Username, "vcap")

			authKeysStat := fs.GetFileTestStat(filepath.Join(sshDirPath, "authorized_keys"))

			assert.NotNil(GinkgoT(), authKeysStat)
			assert.Equal(GinkgoT(), authKeysStat.FileType, fakesys.FakeFileTypeFile)
			assert.Equal(GinkgoT(), authKeysStat.FileMode, os.FileMode(0600))
			assert.Equal(GinkgoT(), authKeysStat.Username, "vcap")
			assert.Equal(GinkgoT(), authKeysStat.Content, "some public key")
		})

		It("centos set user password", func() {
			platform.SetUserPassword("my-user", "my-encrypted-password")
			assert.Equal(GinkgoT(), 1, len(cmdRunner.RunCommands))
			assert.Equal(GinkgoT(), []string{"usermod", "-p", "my-encrypted-password", "my-user"}, cmdRunner.RunCommands[0])
		})

		It("centos setup hostname", func() {
			platform.SetupHostname("foobar.local")
			assert.Equal(GinkgoT(), 1, len(cmdRunner.RunCommands))
			assert.Equal(GinkgoT(), []string{"hostname", "foobar.local"}, cmdRunner.RunCommands[0])

			hostnameFileContent, err := fs.ReadFile("/etc/hostname")
			assert.NoError(GinkgoT(), err)
			assert.Equal(GinkgoT(), "foobar.local", hostnameFileContent)

			hostsFileContent, err := fs.ReadFile("/etc/hosts")
			assert.NoError(GinkgoT(), err)
			assert.Equal(GinkgoT(), CENTOS_EXPECTED_ETC_HOSTS, hostsFileContent)
		})

		It("centos setup dhcp", func() {
			networks := boshsettings.Networks{
				"bosh": boshsettings.Network{
					Default: []string{"dns"},
					Dns:     []string{"xx.xx.xx.xx", "yy.yy.yy.yy", "zz.zz.zz.zz"},
				},
				"vip": boshsettings.Network{
					Default: []string{},
					Dns:     []string{"aa.aa.aa.aa"},
				},
			}

			platform.SetupDhcp(networks)

			dhcpConfig := fs.GetFileTestStat("/etc/dhcp/dhclient.conf")
			assert.NotNil(GinkgoT(), dhcpConfig)
			assert.Equal(GinkgoT(), dhcpConfig.Content, CENTOS_EXPECTED_DHCP_CONFIG)

			assert.Equal(GinkgoT(), len(cmdRunner.RunCommands), 1)
			assert.Equal(GinkgoT(), cmdRunner.RunCommands[0], []string{"service", "network", "restart"})
		})

		It("centos setup dhcp with pre existing configuration", func() {
			fs.WriteToFile("/etc/dhcp/dhclient.conf", CENTOS_EXPECTED_DHCP_CONFIG)
			networks := boshsettings.Networks{
				"bosh": boshsettings.Network{
					Default: []string{"dns"},
					Dns:     []string{"xx.xx.xx.xx", "yy.yy.yy.yy", "zz.zz.zz.zz"},
				},
				"vip": boshsettings.Network{
					Default: []string{},
					Dns:     []string{"aa.aa.aa.aa"},
				},
			}

			platform.SetupDhcp(networks)

			dhcpConfig := fs.GetFileTestStat("/etc/dhcp/dhclient.conf")
			assert.NotNil(GinkgoT(), dhcpConfig)
			assert.Equal(GinkgoT(), dhcpConfig.Content, CENTOS_EXPECTED_DHCP_CONFIG)

			assert.Equal(GinkgoT(), len(cmdRunner.RunCommands), 0)
		})

		It("centos setup manual networking", func() {
			networks := boshsettings.Networks{
				"bosh": boshsettings.Network{
					Default: []string{"dns", "gateway"},
					Ip:      "192.168.195.6",
					Netmask: "255.255.255.0",
					Gateway: "192.168.195.1",
					Mac:     "22:00:0a:1f:ac:2a",
					Dns:     []string{"10.80.130.2", "10.80.130.1"},
				},
			}
			fs.WriteToFile("/sys/class/net/eth0", "")
			fs.WriteToFile("/sys/class/net/eth0/address", "22:00:0a:1f:ac:2a")
			fs.GlobPaths = []string{"/sys/class/net/eth0"}

			platform.SetupManualNetworking(networks)

			networkConfig := fs.GetFileTestStat("/etc/sysconfig/network-scripts/ifcfg-eth0")
			assert.NotNil(GinkgoT(), networkConfig)
			assert.Equal(GinkgoT(), networkConfig.Content, CENTOS_EXPECTED_IFCFG)

			resolvConf := fs.GetFileTestStat("/etc/resolv.conf")
			assert.NotNil(GinkgoT(), resolvConf)
			assert.Equal(GinkgoT(), resolvConf.Content, CENTOS_EXPECTED_RESOLV_CONF)

			time.Sleep(100 * time.Millisecond)

			assert.Equal(GinkgoT(), len(cmdRunner.RunCommands), 7)
			assert.Equal(GinkgoT(), cmdRunner.RunCommands[0], []string{"service", "network", "restart"})
			assert.Equal(GinkgoT(), cmdRunner.RunCommands[1], []string{"arping", "-c", "1", "-U", "-I", "eth0", "192.168.195.6"})
			assert.Equal(GinkgoT(), cmdRunner.RunCommands[6], []string{"arping", "-c", "1", "-U", "-I", "eth0", "192.168.195.6"})
		})

		It("centos setup logrotate", func() {
			platform.SetupLogrotate("fake-group-name", "fake-base-path", "fake-size")

			logrotateFileContent, err := fs.ReadFile("/etc/logrotate.d/fake-group-name")
			assert.NoError(GinkgoT(), err)
			assert.Equal(GinkgoT(), CENTOS_EXPECTED_ETC_LOGROTATE, logrotateFileContent)
		})

		It("centos set time with ntp servers", func() {
			platform.SetTimeWithNtpServers([]string{"0.north-america.pool.ntp.org", "1.north-america.pool.ntp.org"})

			ntpConfig := fs.GetFileTestStat("/fake-dir/bosh/etc/ntpserver")
			assert.Equal(GinkgoT(), "0.north-america.pool.ntp.org 1.north-america.pool.ntp.org", ntpConfig.Content)
			assert.Equal(GinkgoT(), fakesys.FakeFileTypeFile, ntpConfig.FileType)

			assert.Equal(GinkgoT(), 1, len(cmdRunner.RunCommands))
			assert.Equal(GinkgoT(), []string{"ntpdate"}, cmdRunner.RunCommands[0])
		})

		It("centos set time with ntp servers is noop when no ntp server provided", func() {
			platform.SetTimeWithNtpServers([]string{})
			assert.Equal(GinkgoT(), 0, len(cmdRunner.RunCommands))

			ntpConfig := fs.GetFileTestStat("/fake-dir/bosh/etc/ntpserver")
			assert.Nil(GinkgoT(), ntpConfig)
		})

		It("centos setup ephemeral disk with path", func() {
			fakeFormatter := diskManager.FakeFormatter
			fakePartitioner := diskManager.FakePartitioner
			fakeMounter := diskManager.FakeMounter

			fakePartitioner.GetDeviceSizeInMbSizes = map[string]uint64{"/dev/xvda": uint64(1024 * 1024 * 1024)}

			fs.WriteToFile("/dev/xvda", "")

			err := platform.SetupEphemeralDiskWithPath("/dev/xvda")
			assert.NoError(GinkgoT(), err)

			dataDir := fs.GetFileTestStat("/fake-dir/data")
			assert.Equal(GinkgoT(), fakesys.FakeFileTypeDir, dataDir.FileType)
			assert.Equal(GinkgoT(), os.FileMode(0750), dataDir.FileMode)

			assert.Equal(GinkgoT(), "/dev/xvda", fakePartitioner.PartitionDevicePath)
			assert.Equal(GinkgoT(), 2, len(fakePartitioner.PartitionPartitions))

			swapPartition := fakePartitioner.PartitionPartitions[0]
			ext4Partition := fakePartitioner.PartitionPartitions[1]

			assert.Equal(GinkgoT(), "swap", swapPartition.Type)
			assert.Equal(GinkgoT(), "linux", ext4Partition.Type)

			assert.Equal(GinkgoT(), 2, len(fakeFormatter.FormatPartitionPaths))
			assert.Equal(GinkgoT(), "/dev/xvda1", fakeFormatter.FormatPartitionPaths[0])
			assert.Equal(GinkgoT(), "/dev/xvda2", fakeFormatter.FormatPartitionPaths[1])

			assert.Equal(GinkgoT(), 2, len(fakeFormatter.FormatFsTypes))
			assert.Equal(GinkgoT(), boshdisk.FileSystemSwap, fakeFormatter.FormatFsTypes[0])
			assert.Equal(GinkgoT(), boshdisk.FileSystemExt4, fakeFormatter.FormatFsTypes[1])

			assert.Equal(GinkgoT(), 1, len(fakeMounter.MountMountPoints))
			assert.Equal(GinkgoT(), "/fake-dir/data", fakeMounter.MountMountPoints[0])
			assert.Equal(GinkgoT(), 1, len(fakeMounter.MountPartitionPaths))
			assert.Equal(GinkgoT(), "/dev/xvda2", fakeMounter.MountPartitionPaths[0])

			assert.Equal(GinkgoT(), 1, len(fakeMounter.SwapOnPartitionPaths))
			assert.Equal(GinkgoT(), "/dev/xvda1", fakeMounter.SwapOnPartitionPaths[0])

			sysLogStats := fs.GetFileTestStat("/fake-dir/data/sys/log")
			assert.NotNil(GinkgoT(), sysLogStats)
			assert.Equal(GinkgoT(), fakesys.FakeFileTypeDir, sysLogStats.FileType)
			assert.Equal(GinkgoT(), os.FileMode(0750), sysLogStats.FileMode)
			assert.Equal(GinkgoT(), []string{"chown", "root:vcap", "/fake-dir/data/sys"}, cmdRunner.RunCommands[0])
			assert.Equal(GinkgoT(), []string{"chown", "root:vcap", "/fake-dir/data/sys/log"}, cmdRunner.RunCommands[1])

			sysRunStats := fs.GetFileTestStat("/fake-dir/data/sys/run")
			assert.NotNil(GinkgoT(), sysRunStats)
			assert.Equal(GinkgoT(), fakesys.FakeFileTypeDir, sysRunStats.FileType)
			assert.Equal(GinkgoT(), os.FileMode(0750), sysRunStats.FileMode)
			assert.Equal(GinkgoT(), []string{"chown", "root:vcap", "/fake-dir/data/sys/run"}, cmdRunner.RunCommands[2])
		})

		It("centos setup tmp dir", func() {
			err := platform.SetupTmpDir()
			assert.NoError(GinkgoT(), err)

			assert.Equal(GinkgoT(), 2, len(cmdRunner.RunCommands))

			assert.Equal(GinkgoT(), []string{"chown", "root:vcap", "/tmp"}, cmdRunner.RunCommands[0])
			assert.Equal(GinkgoT(), []string{"chmod", "0770", "/tmp"}, cmdRunner.RunCommands[1])
		})

		It("centos mount persistent disk", func() {
			fakeFormatter := diskManager.FakeFormatter
			fakePartitioner := diskManager.FakePartitioner
			fakeMounter := diskManager.FakeMounter

			fs.WriteToFile("/dev/vdf", "")

			err := platform.MountPersistentDisk("/dev/sdf", "/mnt/point")
			assert.NoError(GinkgoT(), err)

			mountPoint := fs.GetFileTestStat("/mnt/point")
			assert.Equal(GinkgoT(), fakesys.FakeFileTypeDir, mountPoint.FileType)
			assert.Equal(GinkgoT(), os.FileMode(0700), mountPoint.FileMode)

			partition := fakePartitioner.PartitionPartitions[0]
			assert.Equal(GinkgoT(), "/dev/vdf", fakePartitioner.PartitionDevicePath)
			assert.Equal(GinkgoT(), 1, len(fakePartitioner.PartitionPartitions))
			assert.Equal(GinkgoT(), "linux", partition.Type)

			assert.Equal(GinkgoT(), 1, len(fakeFormatter.FormatPartitionPaths))
			assert.Equal(GinkgoT(), "/dev/vdf1", fakeFormatter.FormatPartitionPaths[0])

			assert.Equal(GinkgoT(), 1, len(fakeFormatter.FormatFsTypes))
			assert.Equal(GinkgoT(), boshdisk.FileSystemExt4, fakeFormatter.FormatFsTypes[0])

			assert.Equal(GinkgoT(), 1, len(fakeMounter.MountMountPoints))
			assert.Equal(GinkgoT(), "/mnt/point", fakeMounter.MountMountPoints[0])
			assert.Equal(GinkgoT(), 1, len(fakeMounter.MountPartitionPaths))
			assert.Equal(GinkgoT(), "/dev/vdf1", fakeMounter.MountPartitionPaths[0])
		})

		Context("when not mounted", func() {
			It("does not unmount persistent disk ", func() {
				fakeMounter := diskManager.FakeMounter
				fakeMounter.UnmountDidUnmount = false

				fs.WriteToFile("/dev/vdx", "")

				didUnmount, err := platform.UnmountPersistentDisk("/dev/sdx")
				assert.NoError(GinkgoT(), err)
				assert.Equal(GinkgoT(), didUnmount, false)
				assert.Equal(GinkgoT(), "/dev/vdx1", fakeMounter.UnmountPartitionPath)
			})
		})

		Context("when already mounted", func() {
			It("unmounts persistent disk ", func() {
				fakeMounter := diskManager.FakeMounter
				fakeMounter.UnmountDidUnmount = true

				fs.WriteToFile("/dev/vdx", "")

				didUnmount, err := platform.UnmountPersistentDisk("/dev/sdx")
				assert.NoError(GinkgoT(), err)
				assert.Equal(GinkgoT(), didUnmount, true)
				assert.Equal(GinkgoT(), "/dev/vdx1", fakeMounter.UnmountPartitionPath)
			})
		})

		It("centos get real device path with multiple possible devices", func() {
			fs.WriteToFile("/dev/xvda", "")
			fs.WriteToFile("/dev/vda", "")

			realPath, found := platform.NormalizeDiskPath("/dev/sda")
			assert.True(GinkgoT(), found)
			assert.Equal(GinkgoT(), "/dev/xvda", realPath)
		})

		It("centos get real device path with delay within timeout", func() {
			time.AfterFunc(time.Millisecond, func() {
				fs.WriteToFile("/dev/xvda", "")
			})

			realPath, found := platform.NormalizeDiskPath("/dev/sda")
			assert.True(GinkgoT(), found)
			assert.Equal(GinkgoT(), "/dev/xvda", realPath)
		})

		It("centos get real device path with delay beyond timeout", func() {
			time.AfterFunc(2*time.Second, func() {
				fs.WriteToFile("/dev/xvda", "")
			})

			_, found := platform.NormalizeDiskPath("/dev/sda")
			assert.False(GinkgoT(), found)
		})

		It("centos calculate ephemeral disk partition sizes when disk is bigger than twice the memory", func() {
			totalMemInMb := uint64(1024)

			diskSizeInMb := totalMemInMb*2 + 64
			expectedSwap := totalMemInMb
			collector.MemStats.Total = totalMemInMb * uint64(1024*1024)

			fakePartitioner := diskManager.FakePartitioner
			fakePartitioner.GetDeviceSizeInMbSizes = map[string]uint64{
				"/dev/hda": diskSizeInMb,
			}

			err := platform.SetupEphemeralDiskWithPath("/dev/hda")

			assert.NoError(GinkgoT(), err)
			expectedPartitions := []boshdisk.Partition{
				{SizeInMb: expectedSwap, Type: boshdisk.PartitionTypeSwap},
				{SizeInMb: diskSizeInMb - expectedSwap, Type: boshdisk.PartitionTypeLinux},
			}
			assert.Equal(GinkgoT(), fakePartitioner.PartitionPartitions, expectedPartitions)
		})

		It("centos calculate ephemeral disk partition sizes when disk twice the memory or smaller", func() {
			totalMemInMb := uint64(1024)

			diskSizeInMb := totalMemInMb*2 - 64
			expectedSwap := diskSizeInMb / 2

			collector.MemStats.Total = totalMemInMb * uint64(1024*1024)

			fakePartitioner := diskManager.FakePartitioner
			fakePartitioner.GetDeviceSizeInMbSizes = map[string]uint64{
				"/dev/hda": diskSizeInMb,
			}

			err := platform.SetupEphemeralDiskWithPath("/dev/hda")

			assert.NoError(GinkgoT(), err)
			expectedPartitions := []boshdisk.Partition{
				{SizeInMb: expectedSwap, Type: boshdisk.PartitionTypeSwap},
				{SizeInMb: diskSizeInMb - expectedSwap, Type: boshdisk.PartitionTypeLinux},
			}
			assert.Equal(GinkgoT(), fakePartitioner.PartitionPartitions, expectedPartitions)
		})

		It("centos migrate persistent disk", func() {
			fakeMounter := diskManager.FakeMounter

			platform.MigratePersistentDisk("/from/path", "/to/path")

			assert.Equal(GinkgoT(), fakeMounter.RemountAsReadonlyPath, "/from/path")

			assert.Equal(GinkgoT(), 1, len(cmdRunner.RunCommands))
			assert.Equal(GinkgoT(), []string{"sh", "-c", "(tar -C /from/path -cf - .) | (tar -C /to/path -xpf -)"}, cmdRunner.RunCommands[0])

			assert.Equal(GinkgoT(), fakeMounter.UnmountPartitionPath, "/from/path")
			assert.Equal(GinkgoT(), fakeMounter.RemountFromMountPoint, "/to/path")
			assert.Equal(GinkgoT(), fakeMounter.RemountToMountPoint, "/from/path")
		})

		It("centos normalize disk path", func() {
			fs.WriteToFile("/dev/xvda", "")
			path, found := platform.NormalizeDiskPath("/dev/sda")

			assert.Equal(GinkgoT(), path, "/dev/xvda")
			assert.True(GinkgoT(), found)

			fs.RemoveAll("/dev/xvda")
			fs.WriteToFile("/dev/vda", "")
			path, found = platform.NormalizeDiskPath("/dev/sda")

			assert.Equal(GinkgoT(), path, "/dev/vda")
			assert.True(GinkgoT(), found)

			fs.RemoveAll("/dev/vda")
			fs.WriteToFile("/dev/sda", "")
			path, found = platform.NormalizeDiskPath("/dev/sda")

			assert.Equal(GinkgoT(), path, "/dev/sda")
			assert.True(GinkgoT(), found)
		})

		Describe("GetFileContentsFromCDROM", func() {
			It("delegates to cdutil", func() {
				cdutil.GetFileContentsContents = []byte("fake-contents")
				filename := "fake-env"
				contents, err := platform.GetFileContentsFromCDROM(filename)
				Expect(err).NotTo(HaveOccurred())
				Expect(cdutil.GetFileContentsFilename).To(Equal(filename))
				Expect(contents).To(Equal(cdutil.GetFileContentsContents))
			})
		})

		It("centos is device path mounted", func() {
			fs.WriteToFile("/dev/xvda", "")
			fakeMounter := diskManager.FakeMounter
			fakeMounter.IsMountedResult = true

			result, err := platform.IsDevicePathMounted("/dev/sda")
			assert.NoError(GinkgoT(), err)
			assert.True(GinkgoT(), result)
			assert.Equal(GinkgoT(), fakeMounter.IsMountedDevicePathOrMountPoint, "/dev/xvda1")
		})

		It("centos start monit", func() {
			err := platform.StartMonit()
			assert.NoError(GinkgoT(), err)
			assert.Equal(GinkgoT(), 1, len(cmdRunner.RunCommands))
			assert.Equal(GinkgoT(), []string{"sv", "up", "monit"}, cmdRunner.RunCommands[0])
		})

		It("centos setup monit user if file does not exist", func() {
			err := platform.SetupMonitUser()
			assert.NoError(GinkgoT(), err)

			monitUserFileStats := fs.GetFileTestStat("/fake-dir/monit/monit.user")
			assert.NotNil(GinkgoT(), monitUserFileStats)
			assert.Equal(GinkgoT(), "vcap:random-password", monitUserFileStats.Content)
		})

		It("centos setup monit user if file does exist", func() {
			fs.WriteToFile("/fake-dir/monit/monit.user", "vcap:other-random-password")

			err := platform.SetupMonitUser()
			assert.NoError(GinkgoT(), err)

			monitUserFileStats := fs.GetFileTestStat("/fake-dir/monit/monit.user")
			assert.NotNil(GinkgoT(), monitUserFileStats)
			assert.Equal(GinkgoT(), "vcap:other-random-password", monitUserFileStats.Content)
		})

		It("centos get monit credentials reads monit file from disk", func() {
			fs.WriteToFile("/fake-dir/monit/monit.user", "fake-user:fake-random-password")

			username, password, err := platform.GetMonitCredentials()
			assert.NoError(GinkgoT(), err)

			assert.Equal(GinkgoT(), "fake-user", username)
			assert.Equal(GinkgoT(), "fake-random-password", password)
		})

		It("centos get monit credentials errs when invalid file format", func() {
			fs.WriteToFile("/fake-dir/monit/monit.user", "fake-user")

			_, _, err := platform.GetMonitCredentials()
			assert.Error(GinkgoT(), err)
		})

		It("centos get monit credentials leaves colons in password intact", func() {
			fs.WriteToFile("/fake-dir/monit/monit.user", "fake-user:fake:random:password")

			username, password, err := platform.GetMonitCredentials()
			assert.NoError(GinkgoT(), err)

			assert.Equal(GinkgoT(), "fake-user", username)
			assert.Equal(GinkgoT(), "fake:random:password", password)
		})
	})
}
