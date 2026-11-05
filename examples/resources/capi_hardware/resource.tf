# A control-plane machine: its Tinkerbell Hardware, rufio Machine and BMC
# credentials Secret. A machine template selects it by label.
resource "capi_hardware" "cp_1" {
  name      = "cp-1"
  namespace = "capi"
  cluster   = "prod"

  network = {
    mac_address = "aa:bb:cc:dd:ee:01"
    ip_address  = "10.0.80.11"
    netmask     = "255.255.255.0"
    gateway     = "10.0.80.1"
    nameservers = ["10.0.80.1"]
  }
  disk = { device = "/dev/nvme0n1" }
  bmc = {
    address  = "10.0.80.101:16992" # Intel AMT by port; Redfish otherwise
    username = "admin"
    password = var.bmc_passwords["cp-1"]
  }
  labels = { type = "cp" }
}

# A worker without out-of-band management, booted by hand; force lets destroy
# proceed while a TinkerbellMachine still owns it.
resource "capi_hardware" "worker_1" {
  name      = "worker-1"
  namespace = "capi"
  cluster   = "prod"

  network = { mac_address = "aa:bb:cc:dd:ee:11", ip_address = "10.0.80.21", netmask = "255.255.255.0", gateway = "10.0.80.1" }
  disk    = { device = "/dev/sda" }
  labels  = { type = "worker" }
  force   = true
}
