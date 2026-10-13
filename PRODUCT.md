# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

CPA administrators connecting Backboard accounts on local or remote servers.

## Product Purpose

Provide model access through a CPA plugin and let administrators add API-key credentials from a standalone Backboard menu.

## Operating Context

The page follows the existing Zed plugin management-page pattern. Administrators authorize writes with their CPA management key and add one Backboard key at a time.

## Capabilities and Constraints

Multiple keys use separate credentials. New keys are validated before saving; duplicate keys preserve existing settings. The page does not store keys in browser storage. CPA's current plugin SDK cannot add native AI Provider cards or hide its OAuth card independently of credential parsing.
