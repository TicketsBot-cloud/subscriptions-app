package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/TicketsBot-cloud/gdl/objects/interaction"
	"github.com/TicketsBot-cloud/gdl/rest"
)

var commands = []rest.CreateCommandData{
	{
		Name:        "lookup",
		Description: "Look up information about a user's subscription",
		Options: []interaction.ApplicationCommandOption{
			{
				Type:        interaction.OptionTypeString,
				Name:        "email",
				Description: "The Patreon email address of the user to lookup",
				Required:    false,
			},
			{
				Type:        interaction.OptionTypeUser,
				Name:        "user",
				Description: "The Discord Id of the user to lookup",
				Required:    false,
			},
		},
		Type: interaction.ApplicationCommandTypeChatInput,
	},
}

var (
	token = flag.String("token", "", "Bot token")
)

func main() {
	flag.Parse()

	if token == nil || *token == "" {
		panic("no token provided")
	}

	self, err := rest.GetCurrentUser(context.Background(), *token, nil)
	if err != nil {
		panic(err)
	}

	if _, err := rest.ModifyGlobalCommands(context.Background(), *token, nil, self.Id, commands); err != nil {
		panic(err)
	}

	fmt.Println("Commands created successfully")
}
