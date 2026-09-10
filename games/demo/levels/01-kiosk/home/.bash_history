ls
cd /var/catalogue
ls -la
less holds.csv
q
clear
curl -sS -u hleino:{{ .Password "assistant" }} https://intra.kirjasto.example/api/holds -o holds.json
head holds.json
rm holds.json
cd
grep -ri "overdue" /var/catalogue/ | hea
