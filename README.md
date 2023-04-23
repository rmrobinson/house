

## Vision
Current home automation projects, such as HomeAssistant and OpenHAB, offer users the ability to automate common operations and visualize the state of the system through different UIs. These projects include several distinct components:
- a device abstraction layer that allows for the system to interface with a variety of devices
- an automation layer which allows for triggers to be defined with a set of actions to be taken when said trigger activates (in effect, a domain specific policy engine)
- a UI layer which allows the user to interact with and visualize the devices and the automation logic configured in the system.

These projects are designed around the idea that a single system will control a single house, meaning that there are _n_ devices linked to a single device abstraction layer, and the automation layer and UI layer sit on top of these. Automation logic, therefore, is dictating the behaviour of the house and is stored within the house (and references devices specifically in the house). These projects are typically built with fairly tight coupling between the device abstraction layer, the automation layer and the UI layer.

This project takes a different approach. The device abstraction layer exists as a standalone component, and is referred to as the `house` (this project). Within the `house` there are a number of integrations with existing home control systems; the `house` exposes these protocol-agnostic way. The translation between `house` and the existing home control systems is done using `bridges`, which are services that translate `house` data into the appropriate home control framework. The `house` also manages the physical layout of the devices in the home, with the ability to create rooms and floors and then assign devices to locations in the home. The last component that `house` provides is the House Query Language (HQL), a structured way of interacting with the home to allow for generalized retrieval and modification of device sets based on their properties. The `house` runs as a networked service that is intended to be run entirely isolated from the Internet; it makes no outbound network requests and requires no data external to the devices it is managing. Each home will have at least one service fulfilling the `house` contract running.

The automation engine is built as a separate, standalone component which executes a set of configured policies. These policies are built with a set of 'conditions' which may be joined together using basic logical operators (AND, OR, NOT) to provide for fine-grained targeting of the policy. When a policy is triggered, a set of 'actions' are executed using their configured values. It is possible to template in values to be used during action execution. One action type which is supported is the execution of a supplied Lua script; the automation engine framework provides a limited Lua runtime environment with a full implementation of the `house` API contract to allow for advanced actions to be constructed. This automation engine is found at <link to repo>.

A single house will have a separate, standalone automation engine which is locally configured and utilizes the `house` to monitor & control devices in the house. A home will have _n_ control points which will be running software to visualize the state of and control the home; these control point applications may be graphical (built using desktop, web or mobile frameworks) or may be text-based. With a dedicated set of networked APIs exposed by the `house` the creation of different styles of control point will not require complex dependencies.

In addition, dedicated applications will be created to store the state, preferences and automation rules for each individual user. These user control points will be able to 'plug in' to whichever house they happen to currently be in, and will be able to use their set of user-defined rules and preferences to customize the behaviour of the house based on their desires. Each user will have their own, customized UI to view their own automation rules (along with the automation rules of the house they happen to reside in).

Visually, this is intended to look something like the following:

┌─────────────────────┬─────────────────────┐
│ home automation     │ user1 automation    │
├─────────────────────┴─────────────────────┤
│ house                                     │
├──────────┬──────────┬──────────┬──────────┤
│ Zigbee   │ Z-Wave   │ IoTaWatt │ Other    │
└──────────┴──────────┴──────────┴──────────┘