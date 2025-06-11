#!/bin/bash

echo "Setting up Granola to Obsidian sync environment..."

# Create virtual environment
echo "Creating virtual environment..."
python3 -m venv venv

# Activate virtual environment
echo "Activating virtual environment..."
source venv/bin/activate

# Install requirements
echo "Installing requirements..."
pip install -r requirements.txt

echo ""
echo "Setup complete! To run the script:"
echo "1. Activate the virtual environment: source venv/bin/activate"
echo "2. Run the script: python main.py /path/to/your/obsidian/folder"
echo ""
echo "Example: python main.py ~/Documents/MyVault/Granola_Notes" 