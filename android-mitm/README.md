# Setup the MITM to intercept the appie API calls

## Start emulated docker

```bash
docker compose up -d
```

You can visit the android-docker webVNC at [http://localhost:6080](http://localhost:6080)

## Setup the Appie App

1. Download the Appie APK from [apk mirror](https://www.apkmirror.com/apk/albert-heijn/)
2. Rename to zip and extract the apk files
   ```bash
    mv com.icemobile.albertheijn_2024-06-17.apkm ah-app.zip
    unzip ah-app.zip -d ah-app
   ```
3. Install the app on the emulated device
   ```bash
    adb install-multiple ah-app/*.apk
   ```
4. Open the app on the emulated device and log in with your credentials

## Download frida server
```bash
wget https://github.com/frida/frida/releases/download/17.9.3/frida-server-17.9.3-android-x86_64.xz
unxz frida-server-17.9.3-android-x86_64.xz
mv frida-server-17.9.3-android-x86_64 frida-server
adb push frida-server /data/local/tmp/
adb shell "chmod 755 /data/local/tmp/frida-server"
```

## Install frida on the host machine
```bash
python3 -m venv .venv
source .venv/bin/activate
pip install frida-tools
```

## Start frida server on the emulated device
It does not seem possible to run it immeadiately, so will have to run it after setting enforce to 0:
```bash
adb shell
:/ # su
:/ # setenforce 0
:/ # /data/local/tmp/frida-server &
```

## Start intercepting traffic with burp suite
1. Start burp suite and set the proxy to listen on all interfaces with 8080
2. Tell the emulator to use the proxy:
   ```bash
   adb shell settings put global http_proxy <IP_ADDRESS>:8080
   ```
3. Use frida to start the ah app (using venv)
   ```
   frida -U --codeshare akabe1/frida-multiple-unpinning -f com.icemobile.albertheijn
   ```