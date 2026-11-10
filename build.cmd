@echo off
chcp 65001 >nul 2>&1
echo 编译windows版本
call go build -o maven-native-image.exe -ldflags="-s"
call set GOOS=linux
echo 编译linux版本
call go build -o maven-native-image-linux -ldflags="-s"
echo 编译mac版本
call set GOOS=darwin
call go build -o maven-native-image-mac -ldflags="-s"
echo 编译完成
