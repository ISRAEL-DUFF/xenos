-- name: ListNetworkCIDRsAndVLANs :many
SELECT cidr::text AS cidr, vlan_id FROM private_networks;

-- name: CreatePrivateNetwork :one
INSERT INTO private_networks (user_id, name, cidr, vlan_id) VALUES ($1, $2, $3::text::cidr, $4)
RETURNING id, name, cidr::text AS cidr, vlan_id, host, created_at;

-- name: CountUserNetworks :one
SELECT count(*) FROM private_networks WHERE user_id = $1;

-- name: ListUserNetworks :many
SELECT id, name, cidr::text AS cidr, vlan_id, host, created_at FROM private_networks WHERE user_id = $1 ORDER BY id;

-- name: GetUserNetwork :one
SELECT id, name, cidr::text AS cidr, vlan_id, host, created_at FROM private_networks WHERE id = $1 AND user_id = $2;

-- name: LockNetwork :one
SELECT id, cidr::text AS cidr, vlan_id, host FROM private_networks WHERE id = $1 AND user_id = $2 FOR UPDATE;

-- name: DeletePrivateNetwork :one
DELETE FROM private_networks n WHERE n.id = sqlc.arg(id) AND n.user_id = sqlc.arg(user_id)
  AND NOT EXISTS (SELECT 1 FROM vm_private_ips m WHERE m.network_id = n.id)
RETURNING n.cidr::text AS cidr, n.vlan_id;

-- name: PinNetworkHost :exec
UPDATE private_networks SET host = $2 WHERE id = $1;

-- name: UnpinEmptyNetwork :exec
UPDATE private_networks n SET host = NULL WHERE n.id = sqlc.arg(id) AND NOT EXISTS (SELECT 1 FROM vm_private_ips m WHERE m.network_id = n.id);

-- name: NetworkAddressesInUse :many
SELECT host(address)::text AS address FROM vm_private_ips WHERE network_id = $1;

-- name: CountNetworkMembers :one
SELECT count(*) FROM vm_private_ips WHERE network_id = $1;

-- name: AddVMPrivateIP :exec
INSERT INTO vm_private_ips (vm_id, network_id, slot, address, state) VALUES ($1, $2, $3, $4::inet, $5);

-- name: ListVMPrivateIPs :many
SELECT m.id, m.network_id, m.slot, host(m.address)::text AS address, m.state, n.name, n.cidr::text AS cidr, n.vlan_id
FROM vm_private_ips m JOIN private_networks n ON n.id = m.network_id
WHERE m.vm_id = $1 ORDER BY m.slot;

-- name: ListNetworkMembers :many
SELECT m.vm_id, v.hostname, host(m.address)::text AS address, m.state
FROM vm_private_ips m JOIN vms v ON v.id = m.vm_id WHERE m.network_id = $1 ORDER BY m.address;

-- name: SetVMPrivateIPState :exec
UPDATE vm_private_ips SET state = $3 WHERE vm_id = $1 AND network_id = $2;

-- name: DeleteVMPrivateIP :exec
DELETE FROM vm_private_ips WHERE vm_id = $1 AND network_id = $2;

-- name: DeleteVMPrivateIPs :exec
DELETE FROM vm_private_ips WHERE vm_id = $1;

-- name: NetworkIDsOfVM :many
SELECT network_id FROM vm_private_ips WHERE vm_id = $1;

-- name: LockNetworkByID :one
SELECT id FROM private_networks WHERE id = $1 FOR UPDATE;

-- name: QuarantineNetworkIDs :exec
INSERT INTO network_quarantine (cidr, vlan_id, until) VALUES ($1::text::cidr, $2, $3);

-- name: ListQuarantinedNetworkIDs :many
SELECT cidr::text AS cidr, vlan_id FROM network_quarantine WHERE until > now();

-- name: PruneNetworkQuarantine :exec
DELETE FROM network_quarantine WHERE until <= now();
