#!/bin/sh
set -eu

chmod 0600 /home/dsundqvist/forwarding-rule.txt

# The store itself. Readable only by the account that owns it.
sqlite3 /home/dsundqvist/manifests.db \
	"CREATE TABLE manifest(id INTEGER PRIMARY KEY, vessel TEXT, departed TEXT, consignee TEXT);
	 INSERT INTO manifest(vessel, departed, consignee) VALUES
	   ('Nordkapp',  '2024-11-02', 'Meridian Handelsgesellschaft'),
	   ('Sundvaer',  '2024-11-04', 'Meridian Handelsgesellschaft'),
	   ('Havbris',   '2024-11-09', 'Kestrel Shipping Ltd'),
	   ('Nordkapp',  '2024-11-17', 'Meridian Handelsgesellschaft');"
chown dsundqvist:dsundqvist /home/dsundqvist/manifests.db
chmod 0600 /home/dsundqvist/manifests.db
