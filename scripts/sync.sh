#!/usr/bin/env bash

cd synctool

for f in $1/*
do
  go run main.go apply $f -y
done
